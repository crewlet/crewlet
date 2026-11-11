// One MCP server, whole: what it lists across the fleet, and how it has been
// used.

package queries

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcpreview"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
)

// MCPListingReader is the fleet's listings of its MCP servers' tools
// (internal/coord's mcplisting.go) as this node holds them, and the
// definitions they name.
type MCPListingReader interface {
	// MCPListings is every node's listing of every server, whatever the
	// node's presence: the answer filters them by the presence it reads
	// ([coord.LiveMCPListings]). An error is a copy that has not been read
	// yet, never a fleet that lists nothing.
	MCPListings(ctx context.Context) ([]coord.MCPListing, error)

	// MCPToolDefinitions reads the definitions stored under digests, leaving out a
	// digest the store holds nothing under.
	MCPToolDefinitions(ctx context.Context, digests []string) (map[string]coord.MCPToolDefinition, error)

	// MCPAccepted is the definition every server's tools' levels were
	// decided on (internal/coord's mcpaccepted.go), as this node holds
	// them. An error is a copy that has not been read yet, never a fleet
	// that has accepted nothing.
	MCPAccepted(ctx context.Context) ([]coord.MCPAcceptedTool, error)
}

// MCPServerAnswer is `GET /mcp-servers/{name}`: one server, whole, the admin's
// reading; a member is answered [MemberMCPServer] of it.
type MCPServerAnswer struct {
	// Reading is [ReadingAdmin]: which of the answer's two readings this is.
	Reading string `json:"reading"`

	// Server is the server's row exactly as `mcp_servers_status` answers
	// it, and Nodes the live nodes its cells are drawn against.
	Server MCPServerRow    `json:"server"`
	Nodes  []MCPStatusNode `json:"nodes"`

	// Listed is each live node's listing of the server, by node: a node
	// running no instance of it that has listed has none. ALWAYS A LIST.
	Listed []MCPServerListing `json:"listed"`

	// Tools is every tool a live node's instances list, by name, blocked
	// ones included: each once, its instances and nodes summed across the
	// fleet, at the level it runs at. ALWAYS A LIST. ToolCount is how many
	// tools the server lists, as the node counting the most counts them,
	// never fewer than are here: a node names at most
	// [coord.MaxMCPListedTools].
	Tools     []MCPToolView `json:"tools"`
	ToolCount int           `json:"tool_count"`

	// Baseline is the level of every tool the entry's `tools.levels` does
	// not name: its `tools.default`, or null where it sets none (or the
	// configuration does not declare the server), which leaves each such
	// tool at the level its hints start it at ([mcp.StartingLevel]): allow
	// for a tool the server marks read-only, approve for every other.
	Baseline *config.MCPToolLevel `json:"baseline"`

	// UnlistedLevels are the tools `tools.levels` names that no live
	// node's listing names, sorted ([UnlistedLevels]): a level that decides
	// nothing, misspelled or naming a tool the server no longer lists.
	// Empty where the listings cannot say, because no live node lists the
	// server or one lists more tools than its listing names. ALWAYS A LIST.
	UnlistedLevels []string `json:"unlisted_levels"`

	// ServerInfo is what the server said of itself in its handshake, and
	// Instructions what it offers a client for using it, both as the
	// listing changed last gave them: null and "" where no live node's
	// listing carries them. THE SERVER'S OWN WORDS, bounded
	// ([coord.MaxMCPServerInfoBytes], [coord.MaxMCPInstructionsBytes]).
	ServerInfo   *coord.MCPServerInfo `json:"server_info"`
	Instructions string               `json:"instructions"`

	// Dependants is what removing the server takes with it, exactly as a
	// dry run of `DELETE /config/mcp-servers/{name}` answers it
	// ([configapi.Service.ServerDependants]): empty lists for a server the
	// configuration does not declare.
	Dependants configapi.MCPDependants `json:"dependants"`

	// Instances is every instance of the server on the nodes said to hold
	// one, read from each node at query time (ADR-0021), by node then seat;
	// InstanceNodes names each node asked, whether it answered and why not,
	// and how many instances it holds, so a short list is never read as a
	// server with fewer instances. ALWAYS LISTS.
	Instances     []MCPInstanceView     `json:"instances"`
	InstanceNodes []MCPInstanceNodeView `json:"instance_nodes"`

	// LastSuccess is when an instance last answered, and LastFailure the
	// latest failure of any instance read, kept after it came back: null
	// where none has, or no node holding one answered.
	LastSuccess *time.Time      `json:"last_success"`
	LastFailure *MCPFailureView `json:"last_failure"`

	// Resolved is what the engine decides about the server from its
	// configuration: how it signs in, where it runs, the timeouts in force,
	// the integration it reads, the boxes it is given and the placements
	// expected to fail. Null for a server the configuration does not
	// declare, which has nothing to resolve.
	Resolved *MCPResolved `json:"resolved"`

	// Usage is the server's calls per tool over the last [MCPUsageDays]
	// company days, every data node's (the usage domain's counts).
	Usage MCPServerUsage `json:"usage"`

	// Calls is the server's newest calls in the same days, out of the phase
	// records that made them on every data node (turn detail), with which
	// nodes answered.
	Calls MCPServerCalls `json:"calls"`

	// LastCheck is the server's latest test, whoever asked for it through
	// whichever node ([coord.MCPCheck]): null where it was never tested, or
	// where the apply that removed it removed its test with it.
	LastCheck *MCPCheckView `json:"last_check"`

	// Values are the headers or variables the server is sent, by name
	// ([MCPValueState]). Always a list.
	Values []MCPValueState `json:"values"`

	// Reaches is every agent seat the server's grant reaches, each with why
	// ([config.MCPServer.Reach]), in the chart's order. THE GRANT, WHETHER OR
	// NOT THE SERVER IS ON: a server turned off reaches these seats again
	// when it is turned back on, and its row's state says it is off. Always
	// a list; empty for a server the configuration does not declare.
	Reaches []MCPReachView `json:"reaches"`

	// Changes is every change the server made to its tools since their
	// levels were decided, by tool (ADR-0078): a tool it added after its
	// first listing, one it lists under another definition than the one
	// accepted, and one it stopped listing. Each waits for somebody to
	// accept or block it (`POST /mcp-servers/{name}/changes`). Empty for a
	// server whose first listing has not been accepted yet. ALWAYS A LIST,
	// the administrator's reading only.
	Changes []MCPChangeView `json:"changes"`
}

// MCPReachView is one agent seat a server reaches, and why: through `access`
// (every agent, a unit named, the seat named) or, for a server reading each
// agent's own vendor credential, the integration lane the seat is on at. And
// what the limits on the way down to the seat hold the server to for it.
type MCPReachView struct {
	// Seat is the seat's handle.
	Seat   string                `json:"seat"`
	Reason config.MCPReachReason `json:"reason"`
	// Via is the key of the unit that grants it, for `unit`, and the lane the
	// seat is on at, for `integration`; null otherwise.
	Via *string `json:"via"`
	// Limited is what the seat's own `mcp_limits` and every enclosing
	// unit's hold the server to for it ([org.Role.LimitOn]), null where none
	// does.
	Limited *MCPLimitView `json:"limited"`
}

// MCPLimitView is what the limits hold one server to for one agent seat: the
// strictest of the seat's own block and every enclosing unit's, each part
// naming the block nearest the seat that holds it there.
type MCPLimitView struct {
	// Off is whether a limit turns the server off for the seat: its grant
	// stands, and nothing of it starts for the seat or is offered to it.
	Off bool `json:"off"`
	// OffBy is the key of the unit whose limit turns it off, null where the
	// seat's own does or where none does.
	OffBy *string `json:"off_by"`
	// Tools is each tool a limit holds back, by the server's own name for it,
	// sorted. Always a list.
	Tools []MCPToolLimitView `json:"tools"`
}

// MCPToolLimitView is one tool a limit holds back for a seat: at approve, each
// call waits for a person's answer, and at blocked it is never offered. The
// tool runs at the stricter of this and its own level.
type MCPToolLimitView struct {
	// Tool is the server's own name for it, before tool_prefix.
	Tool  string        `json:"tool"`
	Level org.ToolLimit `json:"level"`
	// By is the key of the unit whose limit holds it, null for the seat's
	// own.
	By *string `json:"by"`
}

// reachesOf is every agent seat of roster that server's grant reaches, in the
// chart's order ([org.Organization.AllRoles]), with what its limits hold the
// server to.
func reachesOf(server *config.MCPServer, roster *org.Organization) []MCPReachView {
	out := []MCPReachView{}
	if roster == nil {
		return out
	}
	for seat := range roster.AllRoles() {
		reach, ok := server.Reach(seat)
		if !ok {
			continue
		}
		view := MCPReachView{Seat: seat.Handle(), Reason: reach.Reason}
		if reach.Via != "" {
			view.Via = &reach.Via
		}
		if limit := seat.LimitOn(server.Name); !limit.IsZero() {
			view.Limited = limitView(limit)
		}
		out = append(out, view)
	}
	return out
}

// limitView is the answer's reading of what the limits hold one server to.
func limitView(limit org.ServerLimit) *MCPLimitView {
	unit := func(key string) *string {
		if key == "" {
			return nil
		}
		return &key
	}
	out := &MCPLimitView{Off: limit.Off, Tools: []MCPToolLimitView{}}
	if limit.Off {
		out.OffBy = unit(limit.OffBy)
	}
	for _, tool := range slices.Sorted(maps.Keys(limit.Tools)) {
		held := limit.Tools[tool]
		out.Tools = append(out.Tools, MCPToolLimitView{Tool: tool, Level: held.Level, By: unit(held.By)})
	}
	return out
}

// MCPUsageDays is how many company days a server's usage and recent calls
// cover: a week, today included, so a tool used on one weekday a week still
// shows, and so the calls listed are the calls the counts are of.
const MCPUsageDays = 7

// MCPServerUsage is a server's calls per tool over a range of company days.
type MCPServerUsage struct {
	// Since and Until are the range's first and last company day, both
	// inclusive, and Days its length.
	Since string `json:"since"`
	Until string `json:"until"`
	Days  int    `json:"days"`

	// Tools is one row per tool called in the range, by name. ALWAYS A
	// LIST: a server nobody called has none.
	Tools []MCPToolUsage `json:"tools"`

	// Elided is how many (server, tool) entries the per-seat-day cap
	// dropped across the range, on every data node, entries of which some
	// may have been this server's. Zero says every count is whole.
	Elided int64 `json:"elided"`
}

// MCPToolUsage is one tool's calls over the range: every seat's, every
// node's, each in exactly one of four outcomes (mcp.CallOutcome) at the time
// its turn counted it, so Calls is their sum.
type MCPToolUsage struct {
	// Tool is the name the calls used, prefix included: the
	// [MCPToolView.Name] it joins.
	Tool string `json:"tool"`

	Calls       int64 `json:"calls"`
	Failed      int64 `json:"failed"`
	Unavailable int64 `json:"unavailable"`
	Refused     int64 `json:"refused"`

	// TotalMS is how long the calls that ran took between them: a mean is
	// TotalMS over Calls less Refused, the calls that ran.
	TotalMS int64 `json:"total_ms"`

	// LastAt is the newest call that ran, null when none did.
	LastAt *time.Time `json:"last_at"`

	// Seats are the handles of the seats that called it, sorted. ALWAYS A
	// LIST.
	Seats []string `json:"seats"`
}

// MCPServerCalls is a server's newest calls, at most [store.MaxToolCalls].
type MCPServerCalls struct {
	// Calls is newest first. ALWAYS A LIST.
	Calls []MCPCallRow `json:"calls"`

	// Coverage says which data nodes the calls were read from, and why any
	// did not answer (ADR-0021).
	Coverage eventfan.Coverage `json:"coverage"`
}

// MCPCallRow is one call to one of the server's tools: who, when, which tool
// and how it ended. NEVER ITS ARGUMENTS OR ITS RESULT, which are a model's
// and a server's words and can hold a credential or a customer's data; the
// turn it was made in holds them, for whoever may read that seat's trail.
type MCPCallRow struct {
	// At is when the call was handed to the tool, or for one refused before
	// it ran, when its phase was recorded.
	At time.Time `json:"at"`

	// Seat is the handle of the seat that made it, and TurnID the run it
	// was made in.
	Seat   string `json:"seat"`
	TurnID string `json:"turn_id"`

	Tool string `json:"tool"`

	// Outcome is which of the four the call is in, and Refusal the class of
	// a failure the engine classified, "" otherwise.
	Outcome mcp.CallOutcome `json:"outcome"`
	Refusal mcp.Refusal     `json:"refusal"`

	// DurationMS is how long it took, null for a call nothing timed.
	DurationMS *int64 `json:"duration_ms"`
}

// MCPServerListing is one live node's listing of the server.
type MCPServerListing struct {
	Node string `json:"node"`
	// At is when the node last changed its listing.
	At time.Time `json:"at"`
	// Instances is how many instances on the node the listing is the
	// union of, and Tools how many tools it counts.
	Instances int `json:"instances"`
	Tools     int `json:"tools"`
}

// MCPToolView is one tool the server lists.
type MCPToolView struct {
	// Name is the name it is offered under, the server's tool_prefix
	// applied; Raw is the server's own name for it.
	Name string `json:"name"`
	Raw  string `json:"raw"`
	// Title and Description are the server's words as a listing names
	// them, Description cut to [coord.MaxMCPToolDescriptionBytes]: a list's
	// summary. A change shows them whole ([MCPChangeView]).
	Title       string `json:"title"`
	Description string `json:"description"`
	// Annotations are its hints with the company's overrides applied, as
	// words, as [ToolRow.Annotations] carries them.
	Annotations ToolHints `json:"annotations"`
	// Risk is how much a call can change, RiskDeclared whether a hint the
	// tool carries decided it, and External whether a call may reach past
	// the company: the engine's one reading of the hints ([mcp.RiskOf]),
	// which a screen shows rather than reads again.
	Risk         mcp.Risk `json:"risk"`
	RiskDeclared bool     `json:"risk_declared"`
	External     bool     `json:"external"`
	// Digest names the definition shown (internal/mcp's digest.go), the
	// one most instances list; Definitions counts how many different
	// definitions the fleet lists under the name, 1 unless instances
	// disagree (a server mid-upgrade, or one that serves each seat its
	// own).
	Digest      string `json:"digest"`
	Definitions int    `json:"definitions"`
	// Instances is how many instances across the fleet list it, and Nodes
	// the nodes whose listing names it, sorted. ALWAYS A LIST.
	Instances int      `json:"instances"`
	Nodes     []string `json:"nodes"`
	// InputSchema and OutputSchema are the definition's schemas, null
	// where none is stored: an output schema the server publishes none
	// of, a definition past its bounds or its listing's, or one a sweep
	// removed a moment ago.
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
	// Level is the level the tool runs at, as the server's entry decides
	// it by the server's own name for the tool ([config.MCPToolPolicy.Level])
	// and, where the entry sets neither the tool's level nor a default, as
	// its hints start it ([mcp.StartingLevel], read from the same hints its
	// Risk is); and Custom whether `tools.levels` names it rather than
	// leaving it at the [MCPServerAnswer.Baseline]. A blocked tool is listed
	// and offered to no agent; a tool at approve is offered, and each call
	// waits for a person's answer. For a server the configuration does not
	// declare, what the nodes did: blocked where a listing says no instance
	// filed it, its starting level otherwise.
	Level  config.MCPToolLevel `json:"level"`
	Custom bool                `json:"custom"`
	// Held is a tool whose definition changed since its level was decided
	// (or that the server added since), which its level would let run
	// unasked and which asks a person first until somebody accepts the
	// change: its Level answers approve. Removed is a tool the server no
	// longer lists, kept, named by what was accepted of it, until somebody
	// accepts that it went ([MCPServerAnswer.Changes]).
	Held    bool `json:"held"`
	Removed bool `json:"removed"`

	// starting is the level the tool's hints start it at, which the entry's
	// levels are laid over ([config.MCPToolPolicy.Level]).
	starting config.MCPToolLevel
}

// ErrMCPDetailUnavailable is a detail asked of a node that holds no copy of
// the fleet's listings, no configuration surface to read the dependants from,
// or no history to read the usage and the calls from: one that serves no
// engine.
var ErrMCPDetailUnavailable = errors.New("queries: this node keeps no copy of the fleet's MCP listings or history")

// MCPServer answers one server, whole: `GET /mcp-servers/{name}`.
//
// A ROUTE AND NOT A QUESTION, until a screen watches it: the question registry
// is for answers a room keeps current, and refuses one no room asks for.
//
// THE FLEET'S LISTINGS, not this node's bridges: a per-agent server's children
// run only on the nodes holding its seats, so this node may run none of them.
// Each live node's current listing counts, and only that
// ([coord.LiveMCPListings]): a node that died, drained or restarted stops
// counting with its presence, before any sweep removes its record.
//
// ITS INSTANCES ARE READ WHERE THEY LIVE: every node whose last beat reports
// an instance of it, or that carried no report, is asked at query time
// ([Sources.MCPInstances]), and each is named with whether it answered.
//
// THE ADMIN'S READING, WHOLE: the row names the launch command and the first
// line of each failure, the dependants each block and secret naming the
// server, and the resolved facts the headers or variables a credential goes
// in. A route answers a member [MemberMCPServer] of it ([MCPReading]), as
// `mcp_servers_status` answers a member its own reading. [ErrBadParams] for an
// empty name, [ErrNotFound] for a server no
// node configures, reports or lists, [ErrMCPDetailUnavailable] on a node with
// no copy, and any other error is a store that could not be read or a read of
// the instances this node could not make.
func (s Sources) MCPServer(ctx context.Context, name string) (MCPServerAnswer, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return MCPServerAnswer{}, fmt.Errorf("%w: name must be an MCP server's configured name", ErrBadParams)
	}
	if s.MCPListings == nil || s.Config == nil || s.Coord == nil || s.Usage == nil || s.Events == nil {
		return MCPServerAnswer{}, ErrMCPDetailUnavailable
	}
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	all, err := s.MCPListings.MCPListings(ctx)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	var listings []coord.MCPListing
	for _, l := range coord.LiveMCPListings(all, leases) {
		if l.Server == name {
			listings = append(listings, l)
		}
	}
	grants, err := s.mcpGrants(ctx)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	status := s.mcpStatusOf(leases, grants)
	at := slices.IndexFunc(status.Servers, func(r MCPServerRow) bool { return r.Name == name })
	if at < 0 && len(listings) == 0 {
		return MCPServerAnswer{}, fmt.Errorf("%w: no MCP server %q is configured, reported or listed", ErrNotFound, name)
	}
	out := MCPServerAnswer{
		Reading: ReadingAdmin, Nodes: status.Nodes, Listed: []MCPServerListing{}, Tools: []MCPToolView{},
		Instances: []MCPInstanceView{}, InstanceNodes: []MCPInstanceNodeView{}, Values: []MCPValueState{},
		Reaches: []MCPReachView{}, UnlistedLevels: []string{}, Changes: []MCPChangeView{},
		Dependants: configapi.MCPDependants{MCPCleared: configapi.MCPCleared{
			MCPEnv: []configapi.MCPBlockDependant{}, Sandbox: []configapi.MCPSandboxDependant{},
			MCPLimits: []configapi.MCPBlockDependant{},
		}, Secrets: []string{}},
	}
	if at >= 0 {
		out.Server = status.Servers[at]
	} else {
		// LISTED BY A NODE AND NEITHER CONFIGURED NOR REPORTED: a node
		// on another revision between two beats. The row says so.
		out.Server = MCPServerRow{Name: name, Title: name, Enabled: true, Args: []string{},
			Shared: listings[0].Shared, State: MCPNotStarted, UnlistedAnnotations: []string{},
			RefusedTools: []MCPRefusedTool{}, ShadowedTools: []MCPShadowedTool{}, Nodes: []MCPServerNode{}}
	}
	var organization *org.Organization
	var entry *config.MCPServer
	if s.Company != nil {
		company, roster := s.Company()
		organization = roster
		if company != nil {
			if i := slices.IndexFunc(company.MCPServers, func(m config.MCPServer) bool { return m.Name == name }); i >= 0 {
				entry = &company.MCPServers[i]
				resolved := resolvedOf(company, roster, entry)
				out.Resolved = &resolved
				out.Reaches = reachesOf(entry, roster)
			}
			values, valuesErr := s.mcpValues(ctx, company, name)
			if valuesErr != nil {
				return MCPServerAnswer{}, valuesErr
			}
			out.Values = values
		}
	}
	if out.Server.Configured {
		dependants, found, readErr := s.Config.ServerDependants(ctx, name)
		if readErr != nil {
			return MCPServerAnswer{}, readErr
		}
		if found {
			out.Dependants = dependants
		}
	}
	if s.MCPInstances != nil {
		read, readErr := s.MCPInstances.MCPInstances(ctx, name, instanceNodes(out.Server))
		if readErr != nil {
			return MCPServerAnswer{}, readErr
		}
		out.Instances, out.InstanceNodes, out.LastSuccess, out.LastFailure = instancesOf(read, organization)
	}
	for _, l := range listings {
		out.Listed = append(out.Listed, MCPServerListing{
			Node: l.Node, At: l.At, Instances: l.Instances, Tools: l.ToolCount,
		})
	}
	if latest := latestIdentified(listings); latest != nil {
		out.ServerInfo, out.Instructions = latest.Info, latest.Instructions
	}
	accepted, err := s.MCPListings.MCPAccepted(ctx)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	review := reviewOf(name, listings, accepted, entry)
	tools, counted, kept := serverTools(listings, entry)
	digests := slices.Compact(slices.Sorted(slices.Values(slices.Concat(kept, changeDigests(review.Changes)))))
	definitions, err := s.MCPListings.MCPToolDefinitions(ctx, digests)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	for i := range tools {
		if d, ok := definitions[tools[i].Digest]; ok {
			tools[i].InputSchema, tools[i].OutputSchema = schemaObject(d.Input), schemaObject(d.Output)
		}
	}
	out.Tools, out.ToolCount = tools, counted
	if entry != nil {
		if entry.Tools.Default != "" {
			baseline := entry.Tools.Default
			out.Baseline = &baseline
		}
		out.UnlistedLevels = UnlistedLevels(entry.Tools, out)
	}
	out.Tools = reviewTools(out.Tools, review, definitions)
	out.Changes = changeViews(review, definitions)
	if out.Usage, out.Calls, err = s.mcpActivity(ctx, name); err != nil {
		return MCPServerAnswer{}, err
	}
	if s.MCPChecks != nil {
		check, found, readErr := s.MCPChecks.LatestMCPCheck(ctx, name)
		if readErr != nil {
			return MCPServerAnswer{}, readErr
		}
		if found {
			view := MCPCheckOf(check, organization)
			out.LastCheck = &view
		}
	}
	return out, nil
}

// mcpActivity is a server's usage and newest calls over the last
// [MCPUsageDays] company days: the counts from the usage domain, the calls
// from the phase records on every data node, one window for both.
//
// EACH SEAT BY THE CHART'S HANDLE while the chart holds it, and otherwise by
// the handle its days recorded ([Sources.chartHandles]'s rule); a call by a
// seat neither names is named by the seat's id.
func (s Sources) mcpActivity(ctx context.Context, server string) (MCPServerUsage, MCPServerCalls, error) {
	window := tokens.LastDays(MCPUsageDays, s.clock(), s.zone())
	usageOut := MCPServerUsage{Since: window.First.Label, Until: window.Last.Label,
		Days: window.Days(), Tools: []MCPToolUsage{}}
	rows, err := s.Usage.ServerToolCalls(ctx, usage.ServerToolCallsQuery{
		From: window.First.Label, To: window.Last.Label, Server: server})
	if err != nil {
		return MCPServerUsage{}, MCPServerCalls{}, usageErr(err)
	}
	if usageOut.Elided, err = s.Usage.ToolsElided(ctx, window.First.Label, window.Last.Label); err != nil {
		return MCPServerUsage{}, MCPServerCalls{}, usageErr(err)
	}
	chart := s.chartHandles()
	handles := map[string]string{}
	handleOf := func(agentID string) string {
		if seat, ok := chart[agentID]; ok && seat.handle != "" {
			return seat.handle
		}
		return cmp.Or(handles[agentID], agentID)
	}
	for _, r := range rows {
		if r.Handle != "" {
			handles[r.AgentID] = r.Handle
		}
	}
	usageOut.Tools = foldToolUsage(rows, handleOf)

	calls, coverage, err := s.Events.ToolCalls(ctx, store.ToolCallQuery{
		Server: server, Since: window.First.Start, Limit: store.MaxToolCalls})
	if err != nil {
		return MCPServerUsage{}, MCPServerCalls{}, err
	}
	callsOut := MCPServerCalls{Calls: make([]MCPCallRow, 0, len(calls)), Coverage: coverage}
	for _, c := range calls {
		callsOut.Calls = append(callsOut.Calls, MCPCallRow{
			At: c.At, Seat: handleOf(c.AgentID), TurnID: c.TurnID, Tool: c.Tool,
			Outcome: c.Outcome, Refusal: c.Refusal, DurationMS: c.DurationMS,
		})
	}
	return usageOut, callsOut, nil
}

// foldToolUsage sums every seat's rows into one row per tool, by name, each
// with the handles of the seats that called it.
//
// PURE over its inputs, so the arithmetic is tested without a database.
func foldToolUsage(rows []usage.ServerToolCallsRow, handleOf func(string) string) []MCPToolUsage {
	byTool := map[string]*MCPToolUsage{}
	seats := map[string]map[string]struct{}{}
	for _, r := range rows {
		u, ok := byTool[r.Tool]
		if !ok {
			u = &MCPToolUsage{Tool: r.Tool}
			byTool[r.Tool], seats[r.Tool] = u, map[string]struct{}{}
		}
		u.Calls += r.Calls
		u.Failed += r.Failed
		u.Unavailable += r.Unavailable
		u.Refused += r.Refused
		u.TotalMS += r.TotalMS
		if !r.LastAt.IsZero() && (u.LastAt == nil || r.LastAt.After(*u.LastAt)) {
			last := r.LastAt
			u.LastAt = &last
		}
		seats[r.Tool][handleOf(r.AgentID)] = struct{}{}
	}
	out := make([]MCPToolUsage, 0, len(byTool))
	for _, tool := range slices.Sorted(maps.Keys(byTool)) {
		u := byTool[tool]
		u.Seats = slices.Sorted(maps.Keys(seats[tool]))
		out = append(out, *u)
	}
	return out
}

// latestIdentified is the listing changed last that carries the server's
// identity, nil where none does.
func latestIdentified(listings []coord.MCPListing) *coord.MCPListing {
	var latest *coord.MCPListing
	for i := range listings {
		l := &listings[i]
		if l.Info == nil {
			continue
		}
		if latest == nil || l.At.After(latest.At) {
			latest = l
		}
	}
	return latest
}

// serverTools is every tool one server's live listings name, each once at the
// level it runs at, and how many tools they count in all, never fewer than are
// named (a node names at most [coord.MaxMCPListedTools]); kept are the digests
// whose definitions are stored ([mergeListedTools]).
//
// THE ONE READING OF A SERVER'S TOOLS, which its page lists and each agent's
// count is taken from ([Sources.mcpAgents]), so the two never disagree on which
// tools a server offers. The entry's own decision is laid over what the nodes
// did, each tool at the level the entry gives it by the server's own name or,
// where the entry sets neither, the level its hints start it at
// ([config.MCPToolPolicy.Level]); a server no entry declares (nil) keeps what
// the nodes did.
func serverTools(listings []coord.MCPListing, entry *config.MCPServer) (tools []MCPToolView, counted int,
	kept []string,
) {
	tools, kept = mergeListedTools(listings)
	if entry != nil {
		for i := range tools {
			tools[i].Level, tools[i].Custom = entry.Tools.Level(tools[i].Raw, tools[i].starting)
		}
	}
	counted = len(tools)
	for _, l := range listings {
		counted = max(counted, l.ToolCount)
	}
	return tools, counted, kept
}

// mergeListedTools is every tool the listings name, each once by name, sorted:
// the definition most instances list shown, the instances and nodes summed.
// Its definitions are left for the caller to read, under kept: the digests
// shown whose definitions a listing says are stored, sorted, so a definition
// listed without one costs no read of the store.
func mergeListedTools(listings []coord.MCPListing) (views []MCPToolView, kept []string) {
	type definition struct {
		tool      coord.MCPListedTool
		instances int
		at        time.Time
	}
	byName := map[string]map[string]*definition{}
	nodes := map[string]map[string]struct{}{}
	for _, l := range listings {
		for _, t := range l.Tools {
			if byName[t.Name] == nil {
				byName[t.Name], nodes[t.Name] = map[string]*definition{}, map[string]struct{}{}
			}
			d := byName[t.Name][t.Digest]
			if d == nil {
				d = &definition{tool: t}
				byName[t.Name][t.Digest] = d
			}
			d.instances += t.Instances
			if l.At.After(d.at) {
				d.at = l.At
			}
			nodes[t.Name][l.Node] = struct{}{}
		}
	}
	out := make([]MCPToolView, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		definitions := slices.Collect(maps.Values(byName[name]))
		// THE ONE MOST INSTANCES LIST, then the one listed most recently,
		// then by digest, so every node answers the same definition.
		slices.SortFunc(definitions, func(a, b *definition) int {
			return cmp.Or(cmp.Compare(b.instances, a.instances), b.at.Compare(a.at),
				cmp.Compare(a.tool.Digest, b.tool.Digest))
		})
		instances := 0
		for _, d := range definitions {
			instances += d.instances
		}
		shown := definitions[0].tool
		if shown.DefinitionKept {
			kept = append(kept, shown.Digest)
		}
		// WHAT THE NODES DID, for a server no entry decides: a tool any
		// listing says was left unfiled reads as blocked. The caller lays
		// the entry's own decision over it where the server is declared.
		hints := mcpreview.Annotations(shown.Hints)
		risk := mcp.RiskOf(hints)
		starting := config.MCPToolLevel(mcp.StartingLevel(risk))
		level := starting
		if slices.ContainsFunc(definitions, func(d *definition) bool { return d.tool.Blocked }) {
			level = config.MCPToolBlocked
		}
		out = append(out, MCPToolView{
			Name: shown.Name, Raw: shown.Raw, Title: shown.Title, Description: shown.Description,
			Annotations: ToolHints{
				ReadOnly: hints.ReadOnly.String(), Destructive: hints.Destructive.String(),
				Idempotent: hints.Idempotent.String(), OpenWorld: hints.OpenWorld.String(),
			},
			Risk: risk.Risk, RiskDeclared: risk.Declared, External: risk.External,
			Digest: shown.Digest, Definitions: len(definitions), Instances: instances,
			Nodes: slices.Sorted(maps.Keys(nodes[name])), Level: level, starting: starting,
		})
	}
	slices.Sort(kept)
	return out, slices.Compact(kept)
}

// UnlistedLevels is every tool a policy's `levels` names that the detail's
// listing does not, sorted: a level that decides nothing, because the name is
// misspelled (a prefixed name, where a level takes the server's own) or the
// server no longer lists the tool. Always a list.
//
// JUDGED ONLY WHERE THE LISTING CAN SAY: no live node listing the server says
// nothing about its tools (it is off, or has not started), and a node listing
// more tools than its record names ([coord.MaxMCPListedTools]) may list the
// tool past the cut, so either answers none. A level the fleet's union does
// not name is named by no instance: a per-agent server's children each list
// what their own credentials reach, and one child listing the tool is enough.
func UnlistedLevels(policy config.MCPToolPolicy, detail MCPServerAnswer) []string {
	out := []string{}
	if len(detail.Listed) == 0 || len(detail.Tools) < detail.ToolCount {
		return out
	}
	listed := make(map[string]struct{}, len(detail.Tools))
	for _, t := range detail.Tools {
		listed[t.Raw] = struct{}{}
	}
	for _, tool := range slices.Sorted(maps.Keys(policy.Levels)) {
		if _, ok := listed[tool]; !ok {
			out = append(out, tool)
		}
	}
	return out
}

// schemaObject decodes a stored schema, nil for none or one that is not a
// JSON object.
func schemaObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
