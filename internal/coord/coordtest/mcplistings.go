package coordtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- MCP listings and tool schemas ------------------------------------- //

// aDigest is a definition digest made from seed, as internal/mcp writes one.
func aDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// listing is node's listing of server at epoch, naming tools by their names.
func (h *fleetHarness) listing(server, node string, epoch int64, tools ...string) coord.MCPListing {
	l := coord.MCPListing{
		Server: server, Node: node, Epoch: epoch, At: h.now(), Shared: false, Instances: 2,
		Info:         &coord.MCPServerInfo{Name: server + "-mcp", Version: "1.4.0", Protocol: "2026-07-28"},
		Instructions: "Search before you create.",
		Tools:        []coord.MCPListedTool{},
	}
	for _, name := range tools {
		hints := coord.MCPToolHints{ReadOnly: coord.HintYes, Destructive: coord.HintNo,
			Idempotent: coord.HintUnknown, OpenWorld: coord.HintYes}
		l.Tools = append(l.Tools, coord.MCPListedTool{
			Name: name, Raw: name, Description: "does " + name, Hints: hints, Declared: hints,
			Digest: aDigest(server + "/" + name), DefinitionKept: true, Instances: 2,
			Parts: aParts(server + "/" + name),
		})
	}
	return l
}

// aParts names each part of a definition from seed, as internal/mcp names
// them.
func aParts(seed string) coord.MCPToolParts {
	part := func(which string) string { return aDigest(seed + "#" + which)[:coord.MCPToolPartLength] }
	return coord.MCPToolParts{Title: part("title"), Description: part("description"),
		Schema: part("schema"), Hints: part("hints")}
}

func (h *fleetHarness) putListing(f coord.Fleet, l coord.MCPListing) coord.MCPListing {
	h.t.Helper()
	stored, err := f.PutMCPListing(h.ctx, l)
	if err != nil {
		h.t.Fatalf("PutMCPListing(%s on %s): %v", l.Server, l.Node, err)
	}
	if stored.Version == 0 {
		h.t.Fatalf("PutMCPListing(%s on %s) answered no version", l.Server, l.Node)
	}
	return stored
}

func (h *fleetHarness) listings() []coord.MCPListing {
	h.t.Helper()
	got, err := h.f.ListMCPListings(h.ctx)
	if err != nil {
		h.t.Fatalf("ListMCPListings: %v", err)
	}
	return got
}

// unversioned is a listing as a reader compares it with what was written.
func unversioned(l coord.MCPListing) coord.MCPListing {
	l.Version = 0
	return l
}

var mcpListingCases = []fleetCase{{
	// TWO NODES' LISTINGS OF ONE SERVER ARE TWO RECORDS: each node writes
	// under its own key, so neither can overwrite the other's, and a
	// rewrite by one replaces only its own. Read back from another node's
	// handle, as the fleet reads them.
	name: "each node's listing of a server is its own record, read back whole by every node",
	fn: func(h *fleetHarness) {
		nodes := h.nodes(2)
		a := h.putListing(nodes[0], h.listing("github", "node-a", 3, "get_issue", "create_issue"))
		b := h.putListing(nodes[1], h.listing("github", "node-b", 7, "get_issue"))
		docs := h.putListing(nodes[0], h.listing("docs", "node-a", 3, "search"))
		got, err := nodes[1].ListMCPListings(h.ctx)
		if err != nil {
			h.t.Fatalf("ListMCPListings: %v", err)
		}
		want := []coord.MCPListing{docs, a, b}
		if len(got) != len(want) {
			h.t.Fatalf("listed %d records, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if !reflect.DeepEqual(unversioned(got[i]), unversioned(want[i])) {
				h.t.Errorf("record %d = %+v,\nwant %+v", i, got[i], want[i])
			}
			if got[i].Version != want[i].Version {
				h.t.Errorf("record %d read at version %d, written at %d", i, got[i].Version, want[i].Version)
			}
		}
		// A rewrite by node-a replaces its own listing and nothing else.
		rewritten := h.listing("github", "node-a", 3, "get_issue")
		h.putListing(nodes[0], rewritten)
		for _, l := range h.listings() {
			if l.Server == "github" && l.Node == "node-b" && l.Version != b.Version {
				h.t.Errorf("node-a's rewrite moved node-b's listing to version %d", l.Version)
			}
			if l.Server == "github" && l.Node == "node-a" && len(l.Tools) != 1 {
				h.t.Errorf("node-a's rewrite left %d tools, want 1", len(l.Tools))
			}
		}
	},
}, {
	// A LISTING NAMES EVERY TOOL, BOUNDED BY THE BYTES IT IS STORED AS: a
	// server lists as many tools as it likes with descriptions as long as
	// it likes, and whatever the writer hands over, the record holds every
	// tool by name, each text cut and the words cut further, marked, until
	// the record encodes within MaxMCPListingBytes. A tool a listing did not
	// name could never be reviewed, so a call to it would ask for ever.
	name: "a listing names every tool, its words cut to fit its bytes",
	fn: func(h *fleetHarness) {
		l := h.listing("big", "node-a", 1)
		const total = 1000
		for i := range total {
			name := fmt.Sprintf("tool_%04d", total-i)
			l.Tools = append(l.Tools, coord.MCPListedTool{
				// `<` is stored as six bytes: the bound is on what is stored.
				Name: name, Raw: name, Description: strings.Repeat("<", 3*coord.MaxMCPToolDescriptionBytes),
				Title: strings.Repeat("t", 3*coord.MaxMCPToolTitleBytes),
				Hints: coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
					Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown},
				Declared: coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
					Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown},
				Digest: aDigest(name), Instances: 1, Parts: aParts(name),
			})
		}
		l.Instructions = strings.Repeat("i", 3*coord.MaxMCPInstructionsBytes)
		l.Info.Version = strings.Repeat("v", 3*coord.MaxMCPServerInfoBytes)
		stored := h.putListing(h.f, l)
		got := h.listings()
		if len(got) != 1 {
			h.t.Fatalf("listed %+v", got)
		}
		read := got[0]
		if len(read.Tools) != total {
			h.t.Fatalf("stored %d tools of %d", len(read.Tools), total)
		}
		for i, tool := range read.Tools {
			if want := fmt.Sprintf("tool_%04d", i+1); tool.Name != want || tool.Digest != aDigest(want) {
				h.t.Fatalf("tool %d is %q, want %q by name with its digest", i, tool.Name, want)
			}
		}
		first := read.Tools[0]
		if n := len(first.Description); n > coord.MaxMCPToolDescriptionBytes || !strings.HasSuffix(first.Description, "…") {
			h.t.Errorf("a description travelled as %d bytes, %q", n, first.Description)
		}
		if n := len(first.Title); n > coord.MaxMCPToolTitleBytes {
			h.t.Errorf("a title travelled as %d bytes", n)
		}
		if n := len(read.Instructions); n > coord.MaxMCPInstructionsBytes {
			h.t.Errorf("the instructions travelled as %d bytes", n)
		}
		if n := len(read.Info.Version); n > coord.MaxMCPServerInfoBytes {
			h.t.Errorf("the server's version travelled as %d bytes", n)
		}
		if !reflect.DeepEqual(unversioned(read), unversioned(stored)) {
			h.t.Errorf("the put answered %+v, the listing holds %+v", stored.Tools[0], read.Tools[0])
		}
		raw, err := json.Marshal(read)
		if err != nil {
			h.t.Fatal(err)
		}
		if len(raw) > coord.MaxMCPListingBytes {
			h.t.Errorf("the listing encodes to %d bytes, past %d", len(raw), coord.MaxMCPListingBytes)
		}

		// A FEW TOOLS KEEP THEIR WORDS to their own bounds.
		few := h.listing("small", "node-a", 1)
		few.Tools = append(few.Tools, l.Tools[:3]...)
		if kept := h.putListing(h.f, few).Tools[0]; len(kept.Description) != coord.MaxMCPToolDescriptionBytes {
			h.t.Errorf("three tools' descriptions kept %d bytes, want %d", len(kept.Description),
				coord.MaxMCPToolDescriptionBytes)
		}
	},
}, {
	// A LISTING WHOSE TOOLS' IDENTITIES ALONE ARE PAST ITS BYTES IS REFUSED,
	// saying so, never cut: what it would leave out could never be reviewed.
	name: "a listing whose identities alone are past its bytes is refused, never cut",
	fn: func(h *fleetHarness) {
		l := h.listing("huge", "node-a", 1)
		for i := range 4000 {
			name := fmt.Sprintf("%s%04d", strings.Repeat("<", 100), i)
			l.Tools = append(l.Tools, coord.MCPListedTool{Name: name, Raw: name, Description: "d",
				Hints: coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
					Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown},
				Declared: coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
					Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown},
				Digest: aDigest(name), Instances: 1, Parts: aParts(name)})
		}
		_, err := h.f.PutMCPListing(h.ctx, l)
		if err == nil || !strings.Contains(err.Error(), "refused rather than cut") {
			h.t.Fatalf("a listing past its bytes was answered %v, want a refusal saying it is not cut", err)
		}
		if got := h.listings(); len(got) != 0 {
			h.t.Errorf("a refused listing left %d records", len(got))
		}
	},
}, {
	// A LISTING THE READERS COULD NOT PLACE IS NEVER WRITTEN: one with no
	// epoch would be counted for any incarnation of its node, and a tool
	// named by something that is not a digest names no schema anybody can
	// find.
	name: "a listing with no epoch, a tool with no digest or a hint off the three words is refused",
	fn: func(h *fleetHarness) {
		noEpoch := h.listing("github", "node-a", 0, "get_issue")
		badDigest := h.listing("github", "node-a", 1, "get_issue")
		badDigest.Tools[0].Digest = "not-a-digest"
		badHint := h.listing("github", "node-a", 1, "get_issue")
		badHint.Tools[0].Hints.ReadOnly = "true"
		for name, l := range map[string]coord.MCPListing{
			"no epoch": noEpoch, "bad digest": badDigest, "bad hint": badHint,
		} {
			if _, err := h.f.PutMCPListing(h.ctx, l); err == nil {
				h.t.Errorf("a listing with %s was written", name)
			}
		}
		if got := h.listings(); len(got) != 0 {
			h.t.Errorf("refused listings left %+v", got)
		}
	},
}, {
	// A REMOVAL HOLDS ONLY AT THE VERSION IT READ: the sweep removes a
	// listing it judged stale, and a node that wrote its listing again
	// meanwhile keeps it.
	name: "a listing's removal holds only at the version it read",
	fn: func(h *fleetHarness) {
		first := h.putListing(h.f, h.listing("github", "node-a", 1, "get_issue"))
		second := h.putListing(h.f, h.listing("github", "node-a", 2, "get_issue"))
		if gone, err := h.f.DeleteMCPListing(h.ctx, "github", "node-a", first.Version); err != nil || gone {
			h.t.Fatalf("a removal at a superseded version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteMCPListing(h.ctx, "github", "node-a", 0); err != nil || gone {
			h.t.Fatalf("a removal with no version = (%v, %v), want a lost race", gone, err)
		}
		if gone, err := h.f.DeleteMCPListing(h.ctx, "github", "node-a", second.Version); err != nil || !gone {
			h.t.Fatalf("a removal at the version held = (%v, %v), want it removed", gone, err)
		}
		if got := h.listings(); len(got) != 0 {
			h.t.Errorf("the removed listing is still listed: %+v", got)
		}
	},
}, {
	// THE WATCH IS HOW EVERY NODE KEEPS THE FLEET'S CATALOGUE: every
	// listing, the marker, then each change and each removal, named.
	name: "a listing watch delivers every listing, the marker, then each change and removal",
	fn: func(h *fleetHarness) {
		held := h.putListing(h.f, h.listing("github", "node-a", 1, "get_issue"))
		updates, err := h.f.WatchMCPListings(h.ctx)
		if err != nil {
			h.t.Fatalf("WatchMCPListings: %v", err)
		}
		first := next(h, updates, "listing")
		if first.Listing == nil || first.Server != "github" || first.Node != "node-a" ||
			first.Listing.Version != held.Version {
			h.t.Fatalf("the first answer = %+v, want node-a's github listing", first)
		}
		if marker := next(h, updates, "listing"); !marker.Current || marker.Listing != nil {
			h.t.Fatalf("after the first answer came %+v, want the marker", marker)
		}
		peer := h.nodes(2)[1]
		written := h.putListing(peer, h.listing("github", "node-b", 4, "get_issue", "list_issues"))
		changed := next(h, updates, "listing")
		if changed.Listing == nil || changed.Node != "node-b" || changed.Listing.Version != written.Version ||
			len(changed.Listing.Tools) != 2 {
			h.t.Fatalf("a peer's write arrived as %+v", changed)
		}
		if gone, err := peer.DeleteMCPListing(h.ctx, "github", "node-b", written.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPListing = (%v, %v)", gone, err)
		}
		removed := next(h, updates, "listing")
		if removed.Listing != nil || removed.Server != "github" || removed.Node != "node-b" {
			h.t.Fatalf("the removal arrived as %+v, want node-b's github listing named and nil", removed)
		}
	},
}, {
	// A DEFINITION IS STORED ONCE, under its digest, whoever lists the
	// tool: the first write stores it and every later one is told it is
	// held, leaving it as it was. Its words are kept whole, past the cut a
	// listing makes of them, since a change is shown by them.
	name: "a tool's definition is stored once under its digest, its words whole",
	fn: func(h *fleetHarness) {
		digest := aDigest("github/get_issue")
		schema := coord.MCPToolDefinition{
			Digest: digest, At: h.now(),
			Input:  json.RawMessage(`{"properties":{"number":{"type":"integer"}},"type":"object"}`),
			Output: json.RawMessage(`{"type":"object"}`),
		}
		nodes := h.nodes(2)
		if created, err := nodes[0].CreateMCPToolDefinition(h.ctx, schema); err != nil || !created {
			h.t.Fatalf("the first store = (%v, %v), want it stored", created, err)
		}
		again := schema
		again.At = h.now().Add(time.Hour)
		if created, err := nodes[1].CreateMCPToolDefinition(h.ctx, again); err != nil || created {
			h.t.Fatalf("a second store = (%v, %v), want it held already", created, err)
		}
		read, found, err := nodes[1].MCPToolDefinition(h.ctx, digest)
		if err != nil || !found || !read.At.Equal(h.now()) || read.Version == 0 ||
			string(read.Input) != string(schema.Input) || string(read.Output) != string(schema.Output) {
			h.t.Fatalf("MCPToolDefinition = %+v (found %v, %v), want the first store's", read, found, err)
		}
		all, err := h.f.ListMCPToolDefinitions(h.ctx)
		if err != nil || len(all) != 1 || all[0].Digest != digest {
			h.t.Fatalf("ListMCPToolDefinitions = %+v, %v", all, err)
		}
		if _, found, err := h.f.MCPToolDefinition(h.ctx, aDigest("nothing")); err != nil || found {
			h.t.Fatalf("a digest nothing stored read as (%v, %v)", found, err)
		}
		if gone, err := h.f.DeleteMCPToolDefinition(h.ctx, digest, read.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPToolDefinition = (%v, %v)", gone, err)
		}
		if _, found, _ := h.f.MCPToolDefinition(h.ctx, digest); found {
			h.t.Fatal("a deleted schema is still read")
		}
	},
}, {
	// A DEFINITION PAST ITS BOUNDS IS NEVER STORED, its schemas or its
	// words, and one that is not JSON or is filed under something that is
	// not a digest neither.
	name: "a definition past its bounds, not JSON or under no digest is refused",
	fn: func(h *fleetHarness) {
		big := `{"type":"object","description":"` + strings.Repeat("x", coord.MaxMCPToolSchemaBytes) + `"}`
		for name, s := range map[string]coord.MCPToolDefinition{
			"too large": {Digest: aDigest("big"), At: h.now(), Input: json.RawMessage(big)},
			"too wordy": {Digest: aDigest("wordy"), At: h.now(), Input: json.RawMessage(`{}`),
				Description: strings.Repeat("d", coord.MaxMCPToolWordsBytes-1), Title: "ab"},
			"not JSON":  {Digest: aDigest("bad"), At: h.now(), Input: json.RawMessage(`{`)},
			"no digest": {Digest: "abc", At: h.now(), Input: json.RawMessage(`{}`)},
		} {
			if created, err := h.f.CreateMCPToolDefinition(h.ctx, s); err == nil || created {
				h.t.Errorf("a definition %s was stored (%v, %v)", name, created, err)
			}
		}
	},
}}
