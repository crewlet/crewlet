// What each configured MCP server is doing on each live node.

package queries

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// MCPServerState is one server's condition across the fleet, decided here
// from every node's report so a screen never re-derives it.
//
// EIGHT VALUES, because each calls for something different from the person
// reading it: nothing to do, a node to look at, a server to fix, a grant to
// write, a provisioning pass to let finish, a fleet that has not said, a
// server somebody turned off on purpose, and a sign-in to make.
type MCPServerState string

const (
	// MCPDisabled is a server the configuration turns off
	// (`enabled: false`): nothing is started for it and no seat is granted
	// it, by choice. DECIDED FIRST, before anything a node reports, so a
	// server turned off reads as off rather than as not started, which
	// would send the reader looking for a grant to write; a node still on
	// an earlier revision that runs it shows in its own cell until it
	// applies this one.
	MCPDisabled MCPServerState = "disabled"
	// MCPNeedsSignIn is a server signed in to with OAuth that nobody holds a
	// grant for, or whose grant its vendor refused to renew or let run out:
	// nothing is started for it until somebody signs in, which is the one
	// thing to do. DECIDED AFTER [MCPDisabled] and before anything a node
	// reports, whose counts are its instances held back for it.
	MCPNeedsSignIn MCPServerState = "needs_sign_in"
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
	MCPDisabled, MCPNeedsSignIn, MCPRunning, MCPPartial, MCPFailing, MCPWaiting, MCPNotStarted, MCPUnreported,
}

// Valid reports whether s is a state this build sends.
func (s MCPServerState) Valid() bool { return slices.Contains(MCPServerStates, s) }

// MCPStatusAnswer is `mcp_servers_status`, the admin's reading; a member is
// answered [MemberMCPStatusAnswer].
type MCPStatusAnswer struct {
	// Reading is [ReadingAdmin]: which of the answer's two readings this is.
	Reading string `json:"reading"`
	// Servers in name order: every server the active configuration
	// declares, then any a node reports that it does not (a node still
	// on an older revision, mid-rollout). ALWAYS A LIST.
	Servers []MCPServerRow `json:"servers"`
	// Nodes are the live nodes in id order, each saying whether it
	// reports at all — the columns every server's row is drawn against.
	Nodes []MCPStatusNode `json:"nodes"`
	// Builtin is the engine's own tools as a seat is offered them, on the
	// catalogue's shelves in its order ([builtin.Groups]), each tool a
	// catalogue row naming who may call it. A shelf this company offers no
	// tool of is left out: a company on Jira holds no Worklet tool, and an
	// empty "Work items" would say its agents hold a group they do not.
	// ALWAYS A LIST.
	Builtin []BuiltinGroup `json:"builtin"`
	// Alarms are the MCP alarms standing on this node's latest evaluation,
	// in the table's order, each server with the gesture that fixes it
	// ([MCPAlarm]). ALWAYS A LIST, and empty on a node that runs no state
	// log, which evaluates no alarm.
	Alarms []MCPAlarm `json:"alarms"`
	// CallbackURL is where a vendor sends a person back after a sign-in,
	// and ClientMetadataURL the engine's client metadata document (the
	// client id a cimd sign-in is made as): the two addresses a vendor
	// that allowlists them is given. "" where the engine has no public
	// address.
	CallbackURL       string `json:"callback_url"`
	ClientMetadataURL string `json:"client_metadata_url"`
	// Approvals are the calls agents' runs hold for a person's approval
	// (ADR-0077), the longest waiting first: each while its run waits on
	// it, and once answered until the run carries on to its next job.
	// ALWAYS A LIST, and the administrator's alone: what a held call would
	// send is the seat's trail.
	Approvals []MCPApproval `json:"approvals"`
	// Agents is every agent seat in the chart's order, each with the
	// servers its grants reach and how many MCP tools they hand it
	// ([MCPAgentTools]), and ToolBudget the count past which an agent is
	// flagged ([MCPToolBudget]). Agents is NULL where this node cannot read
	// the fleet's listings yet, which is not an agent holding no tools.
	Agents     []MCPAgentTools `json:"agents"`
	ToolBudget int             `json:"tool_budget"`
}

// MCPApproval is one call to a tool at Needs approval that an agent's run
// holds for a person's answer (`POST /mcp-approvals/{id}`), as an
// administrator reads it.
type MCPApproval struct {
	// ID is what the approval is answered by.
	ID string `json:"id"`
	// At is when the call was held.
	At time.Time `json:"at"`
	// Server is the MCP server, Tool the tool as the run called it (its
	// prefix applied), Raw the server's own name for it, which Always
	// allow sets the level of, and Digest the definition the run was
	// offered.
	Server string `json:"server"`
	Tool   string `json:"tool"`
	Raw    string `json:"raw"`
	Digest string `json:"digest"`
	// Seat is the agent whose run made the call; TurnID and LaunchID the
	// run and the question.
	Seat     string `json:"seat"`
	TurnID   string `json:"turn_id"`
	LaunchID string `json:"launch_id"`
	// Item is the key of the work item the run was on, null where it was on
	// none.
	Item *string `json:"item"`
	// Acting is the decision the call carries out: the newest question the
	// seat asked on that item which a person answered; null for none.
	Acting *sandbox.HeldAct `json:"acting"`
	// Args are what the call would send, opened from their seal: null where
	// the seal does not open on this node.
	Args map[string]any `json:"args"`
	// Status is `waiting` while the run waits on the answer, and
	// `answered` once one is recorded and the run is carried on with it.
	Status MCPApprovalStatus `json:"status"`
	// Answer, AnsweredBy and AnsweredAt are the recorded answer: `once`,
	// `always` or `deny`, who gave it and when, as its seal opens; empty
	// and null while waiting, and where the seal does not open here.
	Answer     string     `json:"answer"`
	AnsweredBy string     `json:"answered_by"`
	AnsweredAt *time.Time `json:"answered_at"`
}

// MCPApprovalStatus is where a held call's answer stands.
type MCPApprovalStatus string

const (
	// MCPApprovalWaiting is a call whose run waits on its answer.
	MCPApprovalWaiting MCPApprovalStatus = "waiting"
	// MCPApprovalAnswered is a call whose answer is recorded, its run being
	// carried on with it.
	MCPApprovalAnswered MCPApprovalStatus = "answered"
)

// MCPApprovalStatuses is every status, for the gate holding a reader's copy.
var MCPApprovalStatuses = []MCPApprovalStatus{MCPApprovalWaiting, MCPApprovalAnswered}

// BuiltinGroup is one shelf of the built-in catalogue.
type BuiltinGroup struct {
	Key builtin.Group `json:"key"`
	// Label and Description are the group's own words, the engine's, so a
	// screen and the API reader name a shelf the same way.
	Label       string `json:"label"`
	Description string `json:"description"`
	// Tools are the shelf's tools in the catalogue's order. ALWAYS A LIST.
	Tools []ToolRow `json:"tools"`
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
	// Title, Catalogue and Vendor are what the curated catalogue recognises
	// the server as, by its address or its command
	// ([catalogue.Recognise]): the entry's title, its key and the vetted
	// mark the dashboard draws. A server it does not recognise is titled by
	// its name, with "" for the other two, which is also a server the
	// configuration does not carry.
	Title     string `json:"title"`
	Catalogue string `json:"catalogue"`
	Vendor    string `json:"vendor"`
	// Configured is whether this node's active configuration declares
	// it. False only for a server a node reports and the configuration no
	// longer (or does not yet) carries.
	Configured bool `json:"configured"`
	// Shared is one instance for the company rather than one per seat
	// that declares credentials for it. From the configuration where it
	// declares the server, else from a node's report.
	Shared bool `json:"shared"`
	// Enabled is whether the configuration runs the server at all
	// (`enabled`, default true); a server it turns off is [MCPDisabled].
	// True on a server the configuration does not carry, which a node is
	// running.
	Enabled bool `json:"enabled"`
	// Description is what the configuration says the server is for, the
	// line each agent's prompt lists beside its name; "" where it says
	// nothing, or does not carry the server.
	Description string `json:"description"`
	// Transport, Command, Args and URL are the launch as configured —
	// the parts that are not credentials (`env` and `headers` are, and
	// are not here). Empty on a server the configuration does not carry.
	Transport string   `json:"transport"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	URL       string   `json:"url"`
	// OAuth is the server's OAuth sign-in, null for a server the
	// configuration does not sign in to with OAuth ([MCPOAuthView]).
	OAuth *MCPOAuthView `json:"oauth"`

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
	// RefusedTools are the tools an instance on a reporting node lists that
	// are not offered for their name: one no model can be handed (a
	// character, the length) or one a built-in holds. ShadowedTools are the
	// ones another server's tool of the same name is offered in place of.
	// Each once across the fleet, sorted by tool, every one any node named
	// (a node names at most [coord.MaxMCPToolNotes] of each). RefusedCount
	// and ShadowedCount are how many there are, as the node counting the
	// most counts them, and never fewer than are listed. ALWAYS LISTS.
	RefusedTools  []MCPRefusedTool  `json:"refused_tools"`
	RefusedCount  int               `json:"refused_count"`
	ShadowedTools []MCPShadowedTool `json:"shadowed_tools"`
	ShadowedCount int               `json:"shadowed_count"`
	// Nodes is one cell per live node, in [MCPStatusAnswer.Nodes] order.
	Nodes []MCPServerNode `json:"nodes"`
}

// MCPRefusedTool is one tool not offered for its name: the name it would be
// filed under, why, and the engine's sentence saying so and what would offer
// it ([mcp.NameRefusal.Sentence]), which a screen shows as written.
type MCPRefusedTool struct {
	Tool    string          `json:"tool"`
	Reason  mcp.NameRefusal `json:"reason"`
	Message string          `json:"message"`
}

// MCPShadowedTool is one tool another server's tool of the same name is
// offered in place of: that server's configured name, and the engine's
// sentence ([mcp.ShadowSentence]).
type MCPShadowedTool struct {
	Tool    string `json:"tool"`
	By      string `json:"by"`
	Message string `json:"message"`
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
	// ErrorClass is why the failed instance Error names failed
	// ([mcp.FailureClass]): "" where no class fits, or a node named one this
	// build does not.
	ErrorClass mcp.FailureClass `json:"error_class"`
	// DownSince is when the longest-failed instance on the node stopped
	// serving, or first failed to start: null when none has failed. What a
	// person reads as how long the server has been down there.
	DownSince *time.Time `json:"down_since"`
	// Restarts is how many times the node brought the server's instances
	// back since each was started from its spec, summed.
	Restarts int `json:"restarts"`
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
// EVERYBODY'S, IN TWO READINGS ([MCPReading]), as each model's condition is: a
// server's state and its tools are what tell a member why an agent lacks one.
// The configuration's reader reads the whole answer, the launch commands, the
// first line of each failure and the servers each agent's grants reach
// included, which is the configuration and what became of it (`/config` shows
// the same reader the same commands); anybody else reads
// [MemberMCPStatusAnswer], which names none of that.
func (s Sources) mcpServersStatus(ctx context.Context, _ Params) (any, error) {
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		// THE LEASE TABLE IS THE ANSWER, so an unreadable one is an
		// error rather than a fleet with no nodes: "no node started
		// github" drawn over a store blip is the wrong thing to act on.
		return nil, err
	}
	grants, err := s.mcpGrants(ctx)
	if err != nil {
		// UNKNOWN, as an unreadable lease table is: "needs a sign-in"
		// drawn over a store blip would send somebody to sign in again.
		return nil, err
	}
	status := s.mcpStatusOf(leases, grants)
	if MCPReading(ctx) != ReadingAdmin {
		// NO ALARM ON A MEMBER'S READING: each one quotes a failure or a
		// wait sentence, which that reading leaves out.
		return MemberMCPStatus(status), nil
	}
	status.Alarms = s.mcpAlarms()
	if status.Approvals, err = s.mcpApprovals(ctx); err != nil {
		return nil, err
	}
	// THE ADMIN'S ALONE, as a seat's tool sources are on the org chart: which
	// servers an agent holds is read off the configuration's grants.
	status.Agents = s.mcpAgents(ctx, leases, status)
	return status, nil
}

// mcpApprovals is every call the fleet's runs hold for a person's approval,
// the longest waiting first, each as [MCPApproval] reads it; empty where this
// node holds no run record.
func (s Sources) mcpApprovals(ctx context.Context) ([]MCPApproval, error) {
	out := []MCPApproval{}
	if s.Sandbox == nil {
		return out, nil
	}
	runs, err := s.Sandbox.ListActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: the runs, which hold the calls waiting for approval, could not "+
			"be read: %w", ErrUnavailable, err)
	}
	for _, run := range runs {
		held := run.HeldCall
		if held == nil || held.LaunchID != run.LaunchID {
			continue
		}
		row := MCPApproval{ID: held.ID, At: held.HeldAt, Server: held.Server, Tool: held.Tool,
			Raw: held.Raw, Digest: held.Digest, Seat: run.AgentHandle, TurnID: run.TurnID,
			LaunchID: run.LaunchID, Args: s.heldArguments(run), Status: MCPApprovalWaiting,
			Acting: held.Acting}
		if item := run.WorkItem; item != nil {
			key := cmp.Or(item.Key, item.ID)
			row.Item = &key
		}
		if run.Answer != nil {
			row.Status = MCPApprovalAnswered
			at := run.Answer.RecordedAt
			row.AnsweredAt = &at
			if s.RunAnswers != nil {
				if words, err := s.RunAnswers.Open(run); err == nil {
					row.Answer, row.AnsweredBy = words.Choice, words.By
				}
			}
		} else if !slices.Contains(sandbox.Awaiting, run.Status) {
			// NOT YET PARKED, or ending: the job made the call and its
			// harness has not ended yet, or the run ends without an answer.
			// Not a call a person can answer yet, or any more.
			continue
		}
		out = append(out, row)
	}
	slices.SortFunc(out, func(a, b MCPApproval) int {
		return cmp.Or(a.At.Compare(b.At), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// mcpStatusOf is `mcp_servers_status` read off the presence leases given and
// the servers' OAuth grants: the one reading of every server's row, which a
// server's detail answers its own row from.
func (s Sources) mcpStatusOf(leases []coord.Lease, grants map[string]*coord.MCPGrant) MCPStatusAnswer {

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
		Reading:    ReadingAdmin,
		Servers:    []MCPServerRow{},
		Nodes:      make([]MCPStatusNode, 0, len(reports)),
		Builtin:    s.builtinGroups(),
		Alarms:     []MCPAlarm{},
		Approvals:  []MCPApproval{},
		Agents:     []MCPAgentTools{},
		ToolBudget: MCPToolBudget,
	}
	if s.MCPGrants != nil {
		out.CallbackURL, out.ClientMetadataURL = s.MCPGrants.MCPSignInAddresses()
	}
	now := s.clock()
	for _, r := range reports {
		out.Nodes = append(out.Nodes, MCPStatusNode{ID: r.node, Reported: r.reported})
	}

	rows := map[string]*MCPServerRow{}
	// prefixes are each configured server's tool_prefix, which a refusal's
	// sentence says how to shorten.
	prefixes := map[string]string{}
	var organization *org.Organization
	if s.Company != nil {
		company, roster := s.Company()
		if company != nil {
			for i := range company.MCPServers {
				server := &company.MCPServers[i]
				row := configuredRow(server)
				row.OAuth = oauthView(server, grants[server.Name], now)
				rows[server.Name] = row
				prefixes[server.Name] = server.ToolPrefix
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
				rows[name] = &MCPServerRow{Name: name, Title: name, Shared: m.Shared, Enabled: true, Args: []string{}}
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
		refused := map[coord.MCPToolRefusal]struct{}{}
		shadowed := map[coord.MCPToolShadow]struct{}{}
		for _, r := range reports {
			cell := MCPServerNode{Node: r.node, Reported: r.reported}
			if m, ok := r.servers[name]; ok {
				cell.Started, cell.Failed, cell.Waiting, cell.Tools = m.Started, m.Failed, m.Waiting, m.Tools
				cell.Error = m.Error
				cell.ErrorSeat = organization.AgentHandleByID(m.ErrorSeat)
				cell.Wait = m.Wait
				cell.WaitSeat = organization.AgentHandleByID(m.WaitSeat)
				if class := mcp.FailureClass(m.ErrorClass); class.Valid() {
					cell.ErrorClass = class
				}
				cell.DownSince, cell.Restarts = instantOrNull(m.DownSince), max(m.Restarts, 0)
				row.Started += m.Started
				row.Failed += m.Failed
				row.Waiting += m.Waiting
				row.Tools = max(row.Tools, m.Tools)
				for _, note := range m.Refused {
					refused[note] = struct{}{}
				}
				for _, note := range m.Shadowed {
					shadowed[note] = struct{}{}
				}
				row.RefusedCount = max(row.RefusedCount, m.RefusedCount)
				row.ShadowedCount = max(row.ShadowedCount, m.ShadowedCount)
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
		row.RefusedTools, row.ShadowedTools = refusedTools(refused, prefixes[name]), shadowedTools(shadowed)
		row.RefusedCount = max(row.RefusedCount, len(row.RefusedTools))
		row.ShadowedCount = max(row.ShadowedCount, len(row.ShadowedTools))
		// A SERVER HELD BACK FOR A SIGN-IN on the nodes needs one too, whatever
		// its grant record says: one made for an address the server is no
		// longer configured at is never handed to it.
		needsSignIn := row.OAuth != nil && (row.OAuth.Status != MCPOAuthConnected ||
			(row.Started == 0 && slices.ContainsFunc(row.Nodes, func(c MCPServerNode) bool {
				return c.Failed > 0 && c.ErrorClass == mcp.FailNeedsSignIn
			})))
		row.State = mcpState(row.Enabled, needsSignIn, anyReported, row.Started, row.Failed, row.Waiting)
		out.Servers = append(out.Servers, *row)
	}
	return out
}

// refusedTools is the fleet's refusals of one server's tools, sorted by tool,
// each worded with prefix, the server's configured tool_prefix.
func refusedTools(notes map[coord.MCPToolRefusal]struct{}, prefix string) []MCPRefusedTool {
	out := make([]MCPRefusedTool, 0, len(notes))
	for note := range notes {
		reason := mcp.NameRefusal(note.Reason)
		out = append(out, MCPRefusedTool{
			Tool: note.Tool, Reason: reason, Message: reason.Sentence(note.Tool, prefix),
		})
	}
	slices.SortFunc(out, func(a, b MCPRefusedTool) int {
		return cmp.Or(cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.Reason, b.Reason))
	})
	return out
}

// shadowedTools is the fleet's shadowed tools of one server, sorted by tool.
func shadowedTools(notes map[coord.MCPToolShadow]struct{}) []MCPShadowedTool {
	out := make([]MCPShadowedTool, 0, len(notes))
	for note := range notes {
		out = append(out, MCPShadowedTool{
			Tool: note.Tool, By: note.By, Message: mcp.ShadowSentence(note.Tool, note.By),
		})
	}
	slices.SortFunc(out, func(a, b MCPShadowedTool) int {
		return cmp.Or(cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.By, b.By))
	})
	return out
}

// configuredRow is a server's launch as the configuration declares it, with
// no credential in it.
func configuredRow(m *config.MCPServer) *MCPServerRow {
	row := &MCPServerRow{
		Name:        m.Name,
		Title:       m.Name,
		Configured:  true,
		Shared:      m.IsShared(),
		Enabled:     m.IsEnabled(),
		Description: m.Description,
		Transport:   string(m.Kind()),
		Command:     m.Command,
		Args:        append([]string{}, m.Args...),
		// ITS CREDENTIALS WITHHELD: an address is shown wherever the
		// server is, and a vendor that takes a key in its query string
		// would otherwise have it answered to every reader of the row.
		URL: redact.URL(m.URL),
	}
	if entry, ok := catalogue.Recognise(m.Launch()); ok {
		row.Title, row.Catalogue, row.Vendor = entry.Title, entry.ID, entry.Vendor
	}
	return row
}

// mcpState is the ONE reading of a server's counts.
//
// A SERVER TURNED OFF IS OFF whatever the counts say ([MCPDisabled]): it is
// the configuration's decision, and the only counts left are a node's that has
// not applied it yet.
//
// A SERVER NOBODY IS SIGNED IN TO NEEDS A SIGN-IN whatever its counts say
// ([MCPNeedsSignIn]): its instances are held back for it, and a sign-in is the
// one gesture that changes them.
//
// A WAITING INSTANCE DECIDES THE STATE ONLY WHERE NOTHING ELSE DOES: a server
// serving some seats while another's credential is minted is running, since
// nothing is wrong with it, and one with a failure is partial or failing
// whatever waits beside it, since the failure is what somebody has to act on.
func mcpState(enabled, needsSignIn, anyReported bool, started, failed, waiting int) MCPServerState {
	switch {
	case !enabled:
		return MCPDisabled
	case needsSignIn:
		return MCPNeedsSignIn
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

// builtinGroups files this node's built-ins on the catalogue's shelves.
//
// A ROW THE CATALOGUE DOES NOT FILE IS LEFT OFF, because there is no shelf to
// draw it on; internal/agent/builtin's sweep fails the build on a built-in a
// seat is offered and the catalogue does not file, so this drops nothing a
// build that passed its tests can offer.
func (s Sources) builtinGroups() []BuiltinGroup {
	out := []BuiltinGroup{}
	if s.Builtins == nil {
		return out
	}
	shelves := map[builtin.Group][]ToolRow{}
	for _, row := range s.Builtins() {
		if row.Group.Valid() {
			shelves[row.Group] = append(shelves[row.Group], row)
		}
	}
	for _, g := range builtin.Groups {
		rows := shelves[g]
		if len(rows) == 0 {
			continue
		}
		slices.SortStableFunc(rows, func(a, b ToolRow) int {
			fa, _ := builtin.FilingOf(a.Name)
			fb, _ := builtin.FilingOf(b.Name)
			return cmp.Compare(fa.Rank, fb.Rank)
		})
		out = append(out, BuiltinGroup{
			Key: g, Label: g.Label(), Description: g.Description(), Tools: rows,
		})
	}
	return out
}
