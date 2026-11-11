package coord_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

func digestOf(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// A LISTING COUNTS ONLY WHILE ITS NODE HOLDS ITS PRESENCE AT THE EPOCH IT WAS
// WRITTEN UNDER: a node that drained or died stops counting the moment its
// presence goes, before any sweep removes its record, and a node that
// restarted, or lapsed and claimed again, is not counted for what its last
// incarnation wrote.
//
// Mutation: filter on the node alone, or count a node with no presence, and
// this is red.
func TestAListingCountsOnlyForItsNodesCurrentIncarnation(t *testing.T) {
	t.Parallel()
	listings := []coord.MCPListing{
		{Server: "github", Node: "node-a", Epoch: 3},
		{Server: "github", Node: "node-b", Epoch: 7},
		{Server: "github", Node: "node-c", Epoch: 2},
		{Server: "docs", Node: "node-a", Epoch: 3},
	}
	presence := []coord.Lease{
		{Resource: coord.NodeResource("node-a"), Epoch: 3},
		// node-b claimed its presence again since it wrote: another
		// incarnation, whose listing is not yet written.
		{Resource: coord.NodeResource("node-b"), Epoch: 8},
		// node-c drained: no presence at all.
		{Resource: coord.SeatResource(uuid.New()), Epoch: 2},
	}
	got := coord.LiveMCPListings(listings, presence)
	if len(got) != 2 || got[0].Node != "node-a" || got[0].Server != "github" ||
		got[1].Node != "node-a" || got[1].Server != "docs" {
		t.Fatalf("LiveMCPListings = %+v, want node-a's two listings in the order given", got)
	}
	if none := coord.LiveMCPListings(listings, nil); len(none) != 0 {
		t.Errorf("with no presence at all, %d listings counted", len(none))
	}
}

// A KEY NAMES ONE NODE'S LISTING OF ONE SERVER AND READS BACK SO, whatever
// either name holds, and a key of another class is not a listing.
func TestAListingKeyReadsBackItsServerAndNode(t *testing.T) {
	t.Parallel()
	key := coord.MCPListingKey("github", "node.a")
	server, node, ok := coord.MCPListingOf(key)
	if !ok || server != "github" || node != "node.a" {
		t.Fatalf("MCPListingOf(%q) = %q, %q, %v", key, server, node, ok)
	}
	for _, other := range []string{coord.PositionKey("node-a"), coord.MCPDefinitionKey(digestOf("x")), "mcp_listing.github"} {
		if _, _, ok := coord.MCPListingOf(other); ok {
			t.Errorf("%q read as a listing key", other)
		}
	}
	if d, ok := coord.MCPDefinitionDigest(coord.MCPDefinitionKey(digestOf("x"))); !ok || d != digestOf("x") {
		t.Errorf("a schema key read back %q, %v", d, ok)
	}
	if _, ok := coord.MCPDefinitionDigest(coord.DocumentKey(coord.MCPDefinitionClass, "nope")); ok {
		t.Error("a schema key naming no digest read as one")
	}
}

// A DEFINITION IS BOUNDED AS IT IS STORED, encoded: `<`, `>` and `&` each take
// six bytes, so words or a schema well inside their bound as raw text can be
// several times past it in the record.
//
// Mutation: measure the raw bytes in Fits or Size, and this is red.
func TestADefinitionIsBoundedAsItIsStored(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	plain := coord.MCPToolDefinition{Digest: digestOf("plain"), Description: "Read one issue.",
		Input: json.RawMessage(`{"type": "object"}`), At: at}
	if !plain.Fits() || plain.Size() != len("Read one issue.")+len(`{"type":"object"}`) {
		t.Fatalf("a plain definition fits %v at %d bytes", plain.Fits(), plain.Size())
	}
	words := plain
	words.Description = strings.Repeat("<", coord.MaxMCPToolWordsBytes/4)
	schema := plain
	schema.Input = json.RawMessage(`{"description": "` + strings.Repeat("&", coord.MaxMCPToolSchemaBytes/4) + `"}`)
	for name, d := range map[string]coord.MCPToolDefinition{"words": words, "schema": schema} {
		if d.Fits() {
			t.Errorf("a definition whose %s are a quarter of their bound as raw text, and past it "+
				"as stored, fits", name)
		}
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if d.Size() > len(raw) || d.Size() < len(raw)-256 {
			t.Errorf("the %s definition weighs %d bytes, and is stored in %d", name, d.Size(), len(raw))
		}
		if err := d.Validate(); err == nil {
			t.Errorf("the %s definition past its bound as stored is written", name)
		}
	}
}
