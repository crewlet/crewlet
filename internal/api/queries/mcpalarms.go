// The MCP alarms standing on this node's latest evaluation, each server with
// the gesture that fixes it.

package queries

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// MCPAlarm is one MCP alarm of this node's latest evaluation (statelog's
// per-server kinds): the table's own words, and every server it stands on.
//
// THE EVALUATION'S, never judged here: the alarm table is evaluated once, on
// the state log's heartbeat, and the health count, the gauge and the log lines
// read that one evaluation, so the page reading it too cannot disagree with
// them about whether a server is down.
type MCPAlarm struct {
	Kind statelog.Kind `json:"kind"`
	// Detail is the longest-standing server's measurement, and Remedy what
	// to do about the kind, both in the table's words.
	Detail string `json:"detail"`
	Remedy string `json:"remedy"`
	// Entries are the servers it stands on, in name order. ALWAYS A LIST.
	Entries []MCPAlarmEntry `json:"entries"`
}

// MCPAlarmEntry is one server an MCP alarm stands on.
type MCPAlarmEntry struct {
	// Key is the server's name in `mcp_servers`.
	Key string `json:"key"`
	// Detail is this server's own measurement.
	Detail string `json:"detail"`
	// Since is when the condition began for this server: when the instance
	// down longest went down, or the instance waiting longest was first held
	// back. Null where the condition carries no instant (a refused name).
	Since *time.Time `json:"since"`
	// Seat is the agent seat, by its handle, whose instance the measurement
	// names; "" for a shared server's instance, or where it names none.
	Seat string `json:"seat"`
	// Fix is the one gesture that fixes it, and what it acts on.
	Fix MCPFix `json:"fix"`
}

// MCPFix is the gesture that fixes one server's alarm: which, and what it acts
// on. DECIDED HERE, from the kind and the entry, so a screen draws the button
// and its words per kind and works out nothing about which server or which
// agent's instance it acts on.
type MCPFix struct {
	Kind MCPFixKind `json:"kind"`
	// Server is the server the gesture acts on, and Seat the one agent's
	// instance it acts on, by handle; "" acts on every instance.
	Server string `json:"server"`
	Seat   string `json:"seat"`
}

// MCPFixKind is a gesture an MCP alarm is fixed by.
type MCPFixKind string

const (
	// MCPFixRestart restarts the server's instances now rather than on the
	// restart schedule (`POST /mcp-servers/{name}/retry`), only the named
	// agent's where [MCPFix.Seat] names one.
	MCPFixRestart MCPFixKind = "restart"
	// MCPFixIntegration opens the integration that mints the named agent's
	// credential for the server, which the server's binding names
	// (`GET /mcp-servers/{name}`'s `resolved.binding`).
	MCPFixIntegration MCPFixKind = "integration"
	// MCPFixEdit opens the server's configuration: a tool name is refused by
	// the server's `tool_prefix` or the server's own naming, never by
	// anything a restart changes.
	MCPFixEdit MCPFixKind = "edit"
	// MCPFixSignIn signs the company in to the server again
	// (`POST /mcp-servers/{name}/oauth`): a grant nobody holds, one its
	// vendor refused to renew, or one about to run out.
	MCPFixSignIn MCPFixKind = "sign_in"
	// MCPFixApprove answers the calls the named agent's run holds for a
	// person's approval on the server, each by its id in the answer's
	// `approvals` (`POST /mcp-approvals/{id}`).
	MCPFixApprove MCPFixKind = "approve"
)

// MCPFixKinds is every gesture, for the gate holding the dashboard's copy.
var MCPFixKinds = []MCPFixKind{MCPFixRestart, MCPFixIntegration, MCPFixEdit, MCPFixSignIn, MCPFixApprove}

// Valid reports whether k is a gesture this build sends.
func (k MCPFixKind) Valid() bool { return slices.Contains(MCPFixKinds, k) }

// mcpFix is how one MCP kind is fixed: the gesture, and whether it acts on the
// one agent's instance the entry names rather than on every instance.
type mcpFix struct {
	kind    MCPFixKind
	oneSeat bool
}

// mcpFixes is the gesture each MCP kind is fixed by. TOTAL OVER THE TABLE'S
// PER-SERVER KINDS, held by a test, so a kind a later piece adds names its
// gesture here or fails the suite rather than reaching the page with none.
//
// A SERVER NOTHING SERVES IS RESTARTED WHOLE, since every instance is down; a
// partial one only where its failure is, so the agents it serves keep their
// sessions. A wait is the integration's to end, a refused name the
// configuration's, and a held call a person's answer.
var mcpFixes = map[statelog.Kind]mcpFix{
	statelog.KindMCPServerFailing:     {kind: MCPFixRestart},
	statelog.KindMCPServerPartial:     {kind: MCPFixRestart, oneSeat: true},
	statelog.KindMCPServerWaitingLong: {kind: MCPFixIntegration, oneSeat: true},
	statelog.KindMCPToolsRefused:      {kind: MCPFixEdit},
	statelog.KindMCPOAuthNeedsSignIn:  {kind: MCPFixSignIn},
	statelog.KindMCPOAuthExpiring:     {kind: MCPFixSignIn},
	statelog.KindMCPApprovalWaiting:   {kind: MCPFixApprove, oneSeat: true},
}

// latestAlarms is this node's latest alarm evaluation, nil where none has run
// or nothing reads one.
func (s Sources) latestAlarms() []statelog.Alarm {
	if s.Alarms == nil {
		return nil
	}
	return s.Alarms()
}

// mcpAlarms is the MCP kinds of this node's latest evaluation, in the table's
// order, each server with its fix. ALWAYS A LIST: empty where none stands,
// and on a node whose table has not been evaluated (one running no state
// log), as `llm_entries` answers its alarms.
func (s Sources) mcpAlarms() []MCPAlarm {
	out := []MCPAlarm{}
	for _, a := range s.latestAlarms() {
		fix, ok := mcpFixes[a.Kind]
		if !ok {
			continue
		}
		alarm := MCPAlarm{Kind: a.Kind, Detail: a.Detail, Remedy: a.Remedy,
			Entries: make([]MCPAlarmEntry, 0, len(a.Entries))}
		for _, e := range a.Entries {
			entry := MCPAlarmEntry{Key: e.Key, Detail: e.Detail, Since: instantOrNull(e.Since), Seat: e.Seat,
				Fix: MCPFix{Kind: fix.kind, Server: e.Key}}
			if fix.oneSeat {
				entry.Fix.Seat = e.Seat
			}
			alarm.Entries = append(alarm.Entries, entry)
		}
		out = append(out, alarm)
	}
	return out
}
