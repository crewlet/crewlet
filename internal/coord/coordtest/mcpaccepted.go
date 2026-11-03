package coordtest

import (
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP accepted definitions ------------------------------------------ //

// accepted is the definition of server's tool accepted by by ("" for a first
// listing's), named from seed.
func (h *fleetHarness) accepted(server, tool, by, seed string) coord.MCPAcceptedTool {
	return coord.MCPAcceptedTool{
		Server: server, Tool: tool, Digest: aDigest(seed), First: by == "", By: by, At: h.now(),
		Title: tool, Description: "does " + tool,
		Hints: coord.MCPToolHints{ReadOnly: coord.HintYes, Destructive: coord.HintNo,
			Idempotent: coord.HintUnknown, OpenWorld: coord.HintNo},
		Parts: aParts(seed),
	}
}

func (h *fleetHarness) putAccepted(f coord.Fleet, a coord.MCPAcceptedTool, version uint64) (coord.MCPAcceptedTool, bool) {
	h.t.Helper()
	stored, wrote, err := f.PutMCPAccepted(h.ctx, a, version)
	if err != nil {
		h.t.Fatalf("PutMCPAccepted(%s on %s at %d): %v", a.Tool, a.Server, version, err)
	}
	if wrote && stored.Version == 0 {
		h.t.Fatalf("PutMCPAccepted(%s on %s) wrote and answered no version", a.Tool, a.Server)
	}
	return stored, wrote
}

func (h *fleetHarness) acceptedRecords() []coord.MCPAcceptedTool {
	h.t.Helper()
	got, err := h.f.ListMCPAccepted(h.ctx)
	if err != nil {
		h.t.Fatalf("ListMCPAccepted: %v", err)
	}
	return got
}

var mcpAcceptedCases = []fleetCase{{
	// THE FIRST LISTING IS ACCEPTED ONCE: two nodes that both believe they
	// list a server first both create, and exactly one record results,
	// the first writer's, which the second cannot replace without having
	// read it.
	name: "an accepted definition is created once, whichever node creates it first",
	fn: func(h *fleetHarness) {
		nodes := h.nodes(2)
		first, wrote := h.putAccepted(nodes[0], h.accepted("github", "get_issue", "", "v1"), 0)
		if !wrote {
			h.t.Fatal("the first create was refused")
		}
		if _, wrote := h.putAccepted(nodes[1], h.accepted("github", "get_issue", "", "v2"), 0); wrote {
			h.t.Fatal("a second create replaced the first node's record")
		}
		got := h.acceptedRecords()
		if len(got) != 1 || got[0].Digest != aDigest("v1") || got[0].Version != first.Version {
			h.t.Fatalf("held %+v, want the first node's v1 alone", got)
		}
	},
}, {
	// A PERSON'S ACCEPTANCE IS A COMPARE-AND-SET on what they read: one
	// written at a version another writer moved is refused, and nothing
	// changes, so two people deciding one change cannot both win.
	name: "an accepted definition is replaced only at the version read",
	fn: func(h *fleetHarness) {
		held, _ := h.putAccepted(h.f, h.accepted("github", "get_issue", "", "v1"), 0)
		moved, wrote := h.putAccepted(h.f, h.accepted("github", "get_issue", "ana", "v2"), held.Version)
		if !wrote || moved.Digest != aDigest("v2") || moved.By != "ana" || moved.First {
			h.t.Fatalf("the update at the version read = %+v (%v)", moved, wrote)
		}
		if _, wrote := h.putAccepted(h.f, h.accepted("github", "get_issue", "ben", "v3"), held.Version); wrote {
			h.t.Fatal("an update at a version since moved was written")
		}
		if _, wrote := h.putAccepted(h.f, h.accepted("github", "list_issues", "ben", "v3"), 99); wrote {
			h.t.Fatal("an update of a record nobody holds was written")
		}
		got := h.acceptedRecords()
		if len(got) != 1 || got[0].Digest != aDigest("v2") || got[0].Version != moved.Version {
			h.t.Fatalf("held %+v, want ana's v2 alone", got)
		}
	},
}, {
	// A REMOVAL ACCEPTED IS A RECORD, naming no definition, so a server
	// that dropped every tool keeps its baseline; and a record that says
	// both is refused.
	name: "an accepted removal keeps a record naming no definition",
	fn: func(h *fleetHarness) {
		held, _ := h.putAccepted(h.f, h.accepted("github", "delete_repo", "", "v1"), 0)
		removal := coord.MCPAcceptedTool{Server: "github", Tool: "delete_repo", Removed: true,
			By: "ana", At: h.now()}
		stored, wrote := h.putAccepted(h.f, removal, held.Version)
		if !wrote || !stored.Removed || stored.Digest != "" {
			h.t.Fatalf("the removal = %+v (%v)", stored, wrote)
		}
		both := h.accepted("github", "delete_repo", "ana", "v2")
		both.Removed = true
		if _, _, err := h.f.PutMCPAccepted(h.ctx, both, stored.Version); err == nil {
			h.t.Error("a removal naming a definition was written")
		}
		unsigned := h.accepted("github", "delete_repo", "ana", "v2")
		unsigned.By = ""
		if _, _, err := h.f.PutMCPAccepted(h.ctx, unsigned, stored.Version); err == nil {
			h.t.Error("a person's acceptance naming nobody was written")
		}
		if got := h.acceptedRecords(); len(got) != 1 || !got[0].Removed {
			h.t.Fatalf("held %+v, want the removal", got)
		}
	},
}, {
	// THE SERVER'S WORDS ARE BOUNDED BY THE STORE, as a listing's are, and
	// the key names the server and the tool whatever the body says.
	name: "an accepted definition is held to its bounds and read back by every node in order",
	fn: func(h *fleetHarness) {
		nodes := h.nodes(2)
		long := h.accepted("wiki", "search", "", "w1")
		long.Description = strings.Repeat("d", 3*coord.MaxMCPToolDescriptionBytes)
		long.Title = strings.Repeat("t", 3*coord.MaxMCPToolTitleBytes)
		h.putAccepted(nodes[0], long, 0)
		h.putAccepted(nodes[0], h.accepted("github", "list_issues", "", "g2"), 0)
		h.putAccepted(nodes[0], h.accepted("github", "get_issue", "", "g1"), 0)
		got, err := nodes[1].ListMCPAccepted(h.ctx)
		if err != nil {
			h.t.Fatalf("ListMCPAccepted: %v", err)
		}
		var order []string
		for _, a := range got {
			order = append(order, a.Server+"/"+a.Tool)
		}
		if strings.Join(order, " ") != "github/get_issue github/list_issues wiki/search" {
			h.t.Fatalf("read in the order %v", order)
		}
		if n := len(got[2].Description); n > coord.MaxMCPToolDescriptionBytes {
			h.t.Errorf("a description travelled as %d bytes", n)
		}
		if n := len(got[2].Title); n > coord.MaxMCPToolTitleBytes {
			h.t.Errorf("a title travelled as %d bytes", n)
		}
	},
}, {
	// A WATCH IS HOW EVERY NODE ENFORCES THE SAME ANSWER: every record, the
	// marker, then each change and removal, whoever made it.
	name: "an accepted-definitions watch delivers every record, the marker, then each change",
	fn: func(h *fleetHarness) {
		held, _ := h.putAccepted(h.f, h.accepted("github", "get_issue", "", "v1"), 0)
		updates, err := h.f.WatchMCPAccepted(h.ctx)
		if err != nil {
			h.t.Fatalf("WatchMCPAccepted: %v", err)
		}
		first := next(h, updates, "accepted definition")
		if first.Accepted == nil || first.Server != "github" || first.Tool != "get_issue" ||
			first.Accepted.Version != held.Version {
			h.t.Fatalf("the first answer = %+v, want github's get_issue", first)
		}
		if marker := next(h, updates, "accepted definition"); !marker.Current || marker.Accepted != nil {
			h.t.Fatalf("after the first answer came %+v, want the marker", marker)
		}
		peer := h.nodes(2)[1]
		moved, _ := h.putAccepted(peer, h.accepted("github", "get_issue", "ana", "v2"), held.Version)
		changed := next(h, updates, "accepted definition")
		if changed.Accepted == nil || changed.Accepted.Digest != aDigest("v2") ||
			changed.Accepted.Version != moved.Version {
			h.t.Fatalf("a peer's acceptance arrived as %+v", changed)
		}
		if gone, err := peer.DeleteMCPAccepted(h.ctx, "github", "get_issue", held.Version); err != nil || gone {
			h.t.Fatalf("a removal at a stale version = (%v, %v), want refused", gone, err)
		}
		if gone, err := peer.DeleteMCPAccepted(h.ctx, "github", "get_issue", moved.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPAccepted = (%v, %v)", gone, err)
		}
		removed := next(h, updates, "accepted definition")
		if removed.Accepted != nil || removed.Server != "github" || removed.Tool != "get_issue" {
			h.t.Fatalf("the removal arrived as %+v, want github's get_issue named and nil", removed)
		}
	},
}}
