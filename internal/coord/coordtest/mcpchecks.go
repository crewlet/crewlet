package coordtest

import (
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- MCP checks ---------------------------------------------------------- //

func (h *fleetHarness) check(server string, at time.Time, by string, nodes ...coord.MCPCheckNode) coord.MCPCheck {
	h.t.Helper()
	c, err := h.f.PutMCPCheck(h.ctx, coord.MCPCheck{
		Server: server, At: at, By: by, ByKind: iam.ActorHuman, OperatorID: sessionOf(by),
		OK: true, Nodes: nodes,
	})
	if err != nil {
		h.t.Fatalf("PutMCPCheck(%s): %v", server, err)
	}
	return c
}

// checkedNode is one node's passing test, as a writer bounds it.
func checkedNode(node string) coord.MCPCheckNode {
	return coord.MCPCheckNode{Node: node, Answered: true, Tools: 3,
		Server: coord.MCPServerInfo{Name: "tracker-mcp", Version: "1.2.0", Protocol: "2026-07-28"},
		Steps: []coord.MCPCheckStep{
			{Stage: "launch", State: "passed", Took: 40 * time.Millisecond, Detail: "the process started"},
			{Stage: "discover", State: "passed", Took: 90 * time.Millisecond, Detail: "tracker-mcp 1.2.0"},
			{Stage: "list", State: "passed", Took: 30 * time.Millisecond, Detail: "3 tools"},
		}}
}

var checkSeat = uuid.MustParse("00000000-0000-7000-8000-00000000000b")

var mcpCheckCases = []fleetCase{{
	// THE CURRENT VALUE ONLY, AND THE LATER TEST WINS: a later test is how
	// the server is now, and an earlier one landing second must not show an
	// older answer on the server's page.
	name: "an MCP check keeps the later test per server, whole",
	fn: func(h *fleetHarness) {
		later := h.check("tracker", h.now().Add(time.Minute), "jane-doe", checkedNode("node-a"), checkedNode("node-b"))
		if later.Version == 0 || later.Server != "tracker" || len(later.Nodes) != 2 ||
			later.Nodes[1].Node != "node-b" || later.Nodes[0].Steps[1].Took != 90*time.Millisecond ||
			later.Nodes[0].Server.Version != "1.2.0" || !later.At.Equal(h.now().Add(time.Minute)) {
			h.t.Fatalf("stored %+v, want jane's two nodes whole, with a version", later)
		}
		if kept := h.check("tracker", h.now(), "omar-ali", checkedNode("node-a")); kept.By != "jane-doe" ||
			kept.Version != later.Version {
			h.t.Fatalf("an earlier test landing second returned %+v, want jane's later one kept", kept)
		}
		seat, err := h.f.PutMCPCheck(h.ctx, coord.MCPCheck{Server: "tracker", Seat: checkSeat,
			At: h.now().Add(2 * time.Minute), By: "omar-ali", ByKind: iam.ActorHuman,
			Nodes: []coord.MCPCheckNode{checkedNode("node-b")}})
		if err != nil || seat.Seat != checkSeat || seat.Version == later.Version {
			h.t.Fatalf("a later check for one seat = %+v, %v; want it to replace the server's", seat, err)
		}
		read, found, err := h.f.MCPCheck(h.ctx, "tracker")
		if err != nil || !found || read.Version != seat.Version || read.Seat != checkSeat || read.OK {
			h.t.Fatalf("MCPCheck(tracker) = %+v (found %v, %v), want omar's seat check", read, found, err)
		}
		if _, found, err := h.f.MCPCheck(h.ctx, "wiki"); found || err != nil {
			h.t.Fatalf("a server never checked reads (found %v, %v)", found, err)
		}
		// Removal is at the version read, so a sweep that read an older
		// record cannot take a newer one away.
		if gone, err := h.f.DeleteMCPCheck(h.ctx, "tracker", later.Version); err != nil || gone {
			h.t.Fatalf("a remove at another record's version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteMCPCheck(h.ctx, "tracker", 0); err != nil || gone {
			h.t.Fatalf("a remove with no version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteMCPCheck(h.ctx, "tracker", seat.Version); err != nil || !gone {
			h.t.Fatalf("a remove at the current version = (%v, %v)", gone, err)
		}
		if _, found, _ := h.f.MCPCheck(h.ctx, "tracker"); found {
			h.t.Fatal("a removed check still reads")
		}
	},
}, {
	// BOUNDED BY ITS WRITER: a record over its bounds is refused rather
	// than cut, so the bound is the writer's to keep, and a refused write
	// leaves nothing behind.
	name: "an MCP check names its server, instant and author, and stays within its bounds",
	fn: func(h *fleetHarness) {
		long := strings.Repeat("x", coord.MaxMCPErrorBytes+1)
		many := make([]coord.MCPCheckNode, coord.MaxMCPCheckNodes+1)
		for i := range many {
			many[i] = checkedNode("node-" + strings.Repeat("n", i+1))
		}
		said := checkedNode("node-a")
		said.Failure = &coord.MCPInstanceFailure{Class: "exited", Said: long, At: h.now()}
		detail := checkedNode("node-a")
		detail.Steps[0].Detail = long
		valid := coord.MCPCheck{Server: "tracker", At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman}
		for name, bad := range map[string]coord.MCPCheck{
			"no server":      {At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			"a slash":        {Server: "a/b", At: h.now(), By: "jane-doe", ByKind: iam.ActorHuman},
			"no instant":     {Server: "tracker", By: "jane-doe", ByKind: iam.ActorHuman},
			"no author":      {Server: "tracker", At: h.now(), ByKind: iam.ActorHuman},
			"no author kind": {Server: "tracker", At: h.now(), By: "jane-doe"},
			"too many nodes": checkWith(valid, many...),
			"a long failure": checkWith(valid, said),
			"a long detail":  checkWith(valid, detail),
			"a nameless node": checkWith(valid, coord.MCPCheckNode{Answered: false,
				Reason: "did not answer"}),
		} {
			if _, err := h.f.PutMCPCheck(h.ctx, bad); err == nil {
				h.t.Errorf("PutMCPCheck with %s was accepted", name)
			}
		}
		if all, _ := h.f.ListMCPChecks(h.ctx); len(all) != 0 {
			h.t.Errorf("a refused check left %+v behind", all)
		}
	},
}, {
	name: "MCP checks list by server",
	fn: func(h *fleetHarness) {
		h.check("wiki", h.now(), "jane-doe", checkedNode("node-a"))
		h.check("chat", h.now(), "jane-doe", checkedNode("node-a"))
		all, err := h.f.ListMCPChecks(h.ctx)
		if err != nil || len(all) != 2 || all[0].Server != "chat" || all[1].Server != "wiki" ||
			all[0].Version == 0 {
			h.t.Fatalf("ListMCPChecks = %+v, %v; want chat then wiki, versioned", all, err)
		}
	},
}}

// checkWith is c naming nodes.
func checkWith(c coord.MCPCheck, nodes ...coord.MCPCheckNode) coord.MCPCheck {
	c.Nodes = nodes
	return c
}
