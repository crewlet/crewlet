// Settings › MCP & tools as a member reads it: each server's condition and its
// tools, and nothing that names a launch line, a header, a variable, an account
// or a call.

package queries

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
)

// MCPReading is which reading of Settings › MCP & tools the principal asking is
// answered: [ReadingAdmin] for a caller the configuration's own verb
// (`config.read`) admits, [ReadingMember] for everybody else.
//
// DECIDED BY THE CONFIGURATION'S ROW, not by a grant read here: the admin
// reading is the configuration and what became of it (each launch line, each
// failure's sentence, the headers and variables a server reads, what removing
// it takes with it), so whoever may read the company document reads it, and a
// change to who may is one change to that row. ONE FUNCTION for the question
// and the routes, so the list and a server's page never answer one caller in
// two readings.
func MCPReading(ctx context.Context) string {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return ReadingMember
	}
	d := authz.Decide(ctx, principal, authz.ActionConfigRead, authz.Object{}, authz.NoChart{},
		authz.DecidedAt(ctx))
	if d.Allowed {
		return ReadingAdmin
	}
	return ReadingMember
}

// MemberMCPStatusAnswer is `mcp_servers_status`, a member's reading.
//
// ITS OWN TYPES, NEVER THE ADMIN'S WITH FIELDS EMPTIED: every struct a member
// is answered is declared here and filled by copying the fields it names
// ([MemberMCPStatus], [MemberMCPServer]), so a field somebody adds to the
// admin's answer reaches no member until it is added here too. The few leaf
// types both readings share say nothing but counts, names and flags, and a
// test walks the member answer's every key against its allow-list, so a field
// added to one of them is a failing build rather than a leak.
type MemberMCPStatusAnswer struct {
	Reading string               `json:"reading"`
	Servers []MemberMCPServerRow `json:"servers"`
	Nodes   []MCPStatusNode      `json:"nodes"`
	// Builtin is the engine's own tools whole: every row is one
	// `GET /tools` already answers everybody.
	Builtin []BuiltinGroup `json:"builtin"`
}

// MemberMCPServerRow is one server, a member's reading: what it is, whether it
// runs, how its instances are doing and which of its tools are not offered,
// with no launch line, no failure's sentence and no wait's.
type MemberMCPServerRow struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Catalogue   string         `json:"catalogue"`
	Vendor      string         `json:"vendor"`
	Configured  bool           `json:"configured"`
	Shared      bool           `json:"shared"`
	Enabled     bool           `json:"enabled"`
	Description string         `json:"description"`
	Transport   string         `json:"transport"`
	State       MCPServerState `json:"state"`
	Started     int            `json:"started"`
	Failed      int            `json:"failed"`
	Waiting     int            `json:"waiting"`
	Tools       int            `json:"tools"`
	// RefusedTools and ShadowedTools are tools the agents are not offered
	// under their names, and why: what tells a member an agent lacks one.
	RefusedTools  []MCPRefusedTool      `json:"refused_tools"`
	RefusedCount  int                   `json:"refused_count"`
	ShadowedTools []MCPShadowedTool     `json:"shadowed_tools"`
	ShadowedCount int                   `json:"shadowed_count"`
	Nodes         []MemberMCPServerNode `json:"nodes"`
}

// MemberMCPServerNode is what one node did with one server, a member's
// reading: the counts, why it failed in a class the page words, and since
// when.
type MemberMCPServerNode struct {
	Node       string           `json:"node"`
	Reported   bool             `json:"reported"`
	Started    int              `json:"started"`
	Failed     int              `json:"failed"`
	Waiting    int              `json:"waiting"`
	Tools      int              `json:"tools"`
	ErrorClass mcp.FailureClass `json:"error_class"`
	DownSince  *time.Time       `json:"down_since"`
	Restarts   int              `json:"restarts"`
}

// MemberMCPServerAnswer is `GET /mcp-servers/{name}`, a member's reading: the
// server's row, every tool it lists, how each instance is doing, what the
// engine resolves about it and how often each tool was called, with no
// failure's sentence, no wait's, no header or variable a credential reaches it
// under, no box, no instructions, nothing removing it would take with it, and
// neither a call nor who made one.
type MemberMCPServerAnswer struct {
	Reading       string                      `json:"reading"`
	Server        MemberMCPServerRow          `json:"server"`
	Nodes         []MCPStatusNode             `json:"nodes"`
	Listed        []MCPServerListing          `json:"listed"`
	Tools         []MemberMCPToolView         `json:"tools"`
	ServerInfo    *MemberMCPServerInfo        `json:"server_info"`
	Instances     []MemberMCPInstanceView     `json:"instances"`
	InstanceNodes []MemberMCPInstanceNodeView `json:"instance_nodes"`
	LastSuccess   *time.Time                  `json:"last_success"`
	LastFailure   *MemberMCPFailureView       `json:"last_failure"`
	Resolved      *MemberMCPResolved          `json:"resolved"`
	Usage         MemberMCPServerUsage        `json:"usage"`
}

// MemberMCPServerUsage is a server's calls per tool over the admin's range of
// company days, a member's reading: how often each tool was called and how the
// calls ended, which tells a member whether a tool is in use, and never which
// seat called it, which is the trail of that seat's work.
type MemberMCPServerUsage struct {
	Since  string               `json:"since"`
	Until  string               `json:"until"`
	Days   int                  `json:"days"`
	Tools  []MemberMCPToolUsage `json:"tools"`
	Elided int64                `json:"elided"`
}

// MemberMCPToolUsage is one tool's calls over the range, a member's reading:
// [MCPToolUsage]'s counts without its seats.
type MemberMCPToolUsage struct {
	Tool        string     `json:"tool"`
	Calls       int64      `json:"calls"`
	Failed      int64      `json:"failed"`
	Unavailable int64      `json:"unavailable"`
	Refused     int64      `json:"refused"`
	TotalMS     int64      `json:"total_ms"`
	LastAt      *time.Time `json:"last_at"`
}

// MemberMCPToolView is one tool the server lists, a member's reading: the
// definition an agent is handed, which the `tools` push already carries to
// everybody, where the fleet lists it, and the level it runs at, which tells a
// member why no agent is offered a blocked one.
type MemberMCPToolView struct {
	Name         string              `json:"name"`
	Raw          string              `json:"raw"`
	Title        string              `json:"title"`
	Description  string              `json:"description"`
	Annotations  ToolHints           `json:"annotations"`
	Risk         mcp.Risk            `json:"risk"`
	RiskDeclared bool                `json:"risk_declared"`
	External     bool                `json:"external"`
	Digest       string              `json:"digest"`
	Definitions  int                 `json:"definitions"`
	Instances    int                 `json:"instances"`
	Nodes        []string            `json:"nodes"`
	InputSchema  map[string]any      `json:"input_schema"`
	OutputSchema map[string]any      `json:"output_schema"`
	Level        config.MCPToolLevel `json:"level"`
	Custom       bool                `json:"custom"`
	Held         bool                `json:"held"`
	Removed      bool                `json:"removed"`
}

// MemberMCPServerInfo is what the server said of itself in its handshake.
type MemberMCPServerInfo struct {
	Name     string `json:"name"`
	Title    string `json:"title,omitempty"`
	Version  string `json:"version,omitempty"`
	Protocol string `json:"protocol,omitempty"`
}

// MemberMCPInstanceView is one instance of the server, a member's reading: its
// seat, node and state, and its failures by class, never by what the server or
// its process said, and never what a waiting one waits for (the variables).
type MemberMCPInstanceView struct {
	Seat        string                 `json:"seat"`
	Node        string                 `json:"node"`
	State       coord.MCPInstanceState `json:"state"`
	Since       *time.Time             `json:"since"`
	Restarts    int                    `json:"restarts"`
	Tools       int                    `json:"tools"`
	Failure     *MemberMCPFailureView  `json:"failure"`
	LastFailure *MemberMCPFailureView  `json:"last_failure"`
	LastAnswer  *time.Time             `json:"last_answer"`
}

// MemberMCPFailureView is one failure of an instance, a member's reading: its
// class, when and where, without the sentence ([MCPFailureView.Said]), which
// is somebody else's text and may name an address or an account.
type MemberMCPFailureView struct {
	Class mcp.FailureClass `json:"class"`
	At    time.Time        `json:"at"`
	Node  string           `json:"node"`
	Seat  string           `json:"seat"`
}

// MemberMCPInstanceNodeView is one node asked for its instances, a member's
// reading: whether it answered and how many it holds, so a short list is never
// read as fewer instances, without the reason one did not answer.
type MemberMCPInstanceNodeView struct {
	Node     string `json:"node"`
	Answered bool   `json:"answered"`
	Count    int    `json:"count"`
}

// MemberMCPResolved is what the engine decides about the server, a member's
// reading: how it signs in, where it runs and the bounds it is held to, and the
// integration it reads, without the headers or variables that credential goes
// in, the seats' boxes or the placements expected to fail.
type MemberMCPResolved struct {
	Auth     MCPAuth           `json:"auth"`
	RunsOn   string            `json:"runs_on"`
	Timeouts MCPTimeouts       `json:"timeouts"`
	Binding  *MemberMCPBinding `json:"binding"`
}

// MemberMCPBinding is the integration a per-agent server reads each agent's
// credential from, and how the engine knows.
type MemberMCPBinding struct {
	Vendor string `json:"vendor"`
	By     string `json:"by"`
}

// MemberMCPStatus is a member's reading of the servers' status: the admin's
// answer, copied field by field into the member's types.
func MemberMCPStatus(a MCPStatusAnswer) MemberMCPStatusAnswer {
	out := MemberMCPStatusAnswer{Reading: ReadingMember, Servers: make([]MemberMCPServerRow, 0, len(a.Servers)),
		Nodes: nonNil(a.Nodes), Builtin: nonNil(a.Builtin)}
	for _, row := range a.Servers {
		out.Servers = append(out.Servers, memberRow(row))
	}
	return out
}

// MemberMCPServer is a member's reading of one server's detail: the admin's
// answer, copied field by field into the member's types.
func MemberMCPServer(a MCPServerAnswer) MemberMCPServerAnswer {
	out := MemberMCPServerAnswer{Reading: ReadingMember, Server: memberRow(a.Server), Nodes: nonNil(a.Nodes),
		Listed: nonNil(a.Listed), Tools: make([]MemberMCPToolView, 0, len(a.Tools)),
		Instances:     make([]MemberMCPInstanceView, 0, len(a.Instances)),
		InstanceNodes: make([]MemberMCPInstanceNodeView, 0, len(a.InstanceNodes)),
		LastSuccess:   a.LastSuccess, LastFailure: memberFailure(a.LastFailure),
		Usage: MemberMCPServerUsage{Since: a.Usage.Since, Until: a.Usage.Until, Days: a.Usage.Days,
			Tools: make([]MemberMCPToolUsage, 0, len(a.Usage.Tools)), Elided: a.Usage.Elided}}
	for _, u := range a.Usage.Tools {
		out.Usage.Tools = append(out.Usage.Tools, MemberMCPToolUsage{Tool: u.Tool, Calls: u.Calls,
			Failed: u.Failed, Unavailable: u.Unavailable, Refused: u.Refused, TotalMS: u.TotalMS,
			LastAt: u.LastAt})
	}
	for _, t := range a.Tools {
		out.Tools = append(out.Tools, MemberMCPToolView{Name: t.Name, Raw: t.Raw, Title: t.Title,
			Description: t.Description, Annotations: t.Annotations, Risk: t.Risk, RiskDeclared: t.RiskDeclared,
			External: t.External, Digest: t.Digest, Definitions: t.Definitions, Instances: t.Instances,
			Nodes: nonNil(t.Nodes), InputSchema: t.InputSchema, OutputSchema: t.OutputSchema,
			Level: t.Level, Custom: t.Custom, Held: t.Held, Removed: t.Removed})
	}
	if info := a.ServerInfo; info != nil {
		out.ServerInfo = &MemberMCPServerInfo{Name: info.Name, Title: info.Title, Version: info.Version,
			Protocol: info.Protocol}
	}
	for _, in := range a.Instances {
		out.Instances = append(out.Instances, MemberMCPInstanceView{Seat: in.Seat, Node: in.Node, State: in.State,
			Since: in.Since, Restarts: in.Restarts, Tools: in.Tools, Failure: memberFailure(in.Failure),
			LastFailure: memberFailure(in.LastFailure), LastAnswer: in.LastAnswer})
	}
	for _, n := range a.InstanceNodes {
		out.InstanceNodes = append(out.InstanceNodes, MemberMCPInstanceNodeView{Node: n.Node,
			Answered: n.Answered, Count: n.Count})
	}
	if r := a.Resolved; r != nil {
		resolved := &MemberMCPResolved{Auth: r.Auth, RunsOn: r.RunsOn, Timeouts: r.Timeouts}
		if b := r.Binding; b != nil {
			resolved.Binding = &MemberMCPBinding{Vendor: b.Vendor, By: b.By}
		}
		out.Resolved = resolved
	}
	return out
}

// memberRow is one server's row, a member's reading.
func memberRow(r MCPServerRow) MemberMCPServerRow {
	out := MemberMCPServerRow{Name: r.Name, Title: r.Title, Catalogue: r.Catalogue, Vendor: r.Vendor,
		Configured: r.Configured, Shared: r.Shared, Enabled: r.Enabled, Description: r.Description,
		Transport: r.Transport, State: r.State, Started: r.Started, Failed: r.Failed, Waiting: r.Waiting,
		Tools: r.Tools, RefusedTools: nonNil(r.RefusedTools), RefusedCount: r.RefusedCount,
		ShadowedTools: nonNil(r.ShadowedTools), ShadowedCount: r.ShadowedCount,
		Nodes: make([]MemberMCPServerNode, 0, len(r.Nodes))}
	for _, c := range r.Nodes {
		out.Nodes = append(out.Nodes, MemberMCPServerNode{Node: c.Node, Reported: c.Reported, Started: c.Started,
			Failed: c.Failed, Waiting: c.Waiting, Tools: c.Tools, ErrorClass: c.ErrorClass,
			DownSince: c.DownSince, Restarts: c.Restarts})
	}
	return out
}

// memberFailure is one failure, a member's reading: nil for none.
func memberFailure(f *MCPFailureView) *MemberMCPFailureView {
	if f == nil {
		return nil
	}
	return &MemberMCPFailureView{Class: f.Class, At: f.At, Node: f.Node, Seat: f.Seat}
}
