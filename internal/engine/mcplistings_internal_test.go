package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// listingCompany is a shared server serving `search`; a per-agent server two
// seats each start a child of, whose child lists `probe_<launch>`, so the two
// children list different tools; and a per-agent server whose two children
// both list `lookup`.
func listingCompany(t *testing.T) *Company {
	t.Helper()
	return specCompany(t, fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:`+standIn("docs", "search", true)+standIn("wiki", "lookup", false)+`
  - name: tracker
    shared: false
    command: %q
    env:
      %s: "server"
      %s: "probe_%s"
      %s: %q
      %s: %q
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-token"
      wiki:
        SEAT_TOKEN: "ceo-token"
  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "cto-token"
      wiki:
        SEAT_TOKEN: "cto-token"
`, os.Args[0], ToolServerModeEnv, ToolServerToolEnv, ToolServerLaunchToken,
		ToolServerLaunchesEnv, t.TempDir(), procgrouptest.GORACE, procgrouptest.StandInRaceOptions()))
}

// A NODE'S LISTING OF A SERVER IS THE UNION OF ITS INSTANCES': a per-agent
// server's children list what each seat's credentials reach, so the node
// names every tool any of them lists, each counted by the instances listing
// it, with the schemas each names; and a shared server is one instance.
//
// Mutation: keep the first instance's listing alone, or count a tool once
// however many instances list it, and this is red.
func TestANodesListingIsTheUnionOfItsInstances(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := listingCompany(t)
	e := &Engine{}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { _, _ = e.mcp.StopAll(context.Background()) })
	e.epoch.current.Store(company)
	e.startSharedServers(ctx, company)
	for _, seat := range []string{"ceo", "cto"} {
		e.startSeatServers(ctx, seat)
		t.Cleanup(func() { e.stopSeatServers(context.Background(), seat) })
	}

	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	listings := e.nodeMCPListings("node-a", 4, now)
	if len(listings) != 3 {
		t.Fatalf("listings for %d servers, want docs, tracker and wiki", len(listings))
	}
	tracker := listings["tracker"].listing
	if tracker.Shared || tracker.Instances != 2 || tracker.Node != "node-a" || tracker.Epoch != 4 ||
		!tracker.At.Equal(now) {
		t.Errorf("tracker's listing = %+v, want two instances of a per-agent server", tracker)
	}
	var names []string
	for _, tool := range tracker.Tools {
		names = append(names, tool.Name)
		if tool.Instances != 1 || !coord.IsMCPDigest(tool.Digest) || !tool.SchemaKept {
			t.Errorf("tracker's %s = %+v, want one instance listing it and its schema kept", tool.Name, tool)
		}
		if _, ok := listings["tracker"].schemas[tool.Digest]; !ok {
			t.Errorf("tracker's %s names a schema its listing does not carry", tool.Name)
		}
	}
	if !slices.Equal(names, []string{"probe_1", "probe_2"}) || tracker.ToolCount != 2 {
		t.Errorf("tracker lists %v of %d, want both children's tools", names, tracker.ToolCount)
	}
	if err := tracker.Validate(); err != nil {
		t.Errorf("the derived listing is one the store refuses: %v", err)
	}
	docs := listings["docs"].listing
	if !docs.Shared || docs.Instances != 1 || len(docs.Tools) != 1 || docs.Tools[0].Name != "search" {
		t.Errorf("docs' listing = %+v, want the shared instance's one tool", docs)
	}
	// ONE TOOL BOTH CHILDREN LIST is named once, counted by both.
	wiki := listings["wiki"].listing
	if wiki.Shared || wiki.Instances != 2 || len(wiki.Tools) != 1 || wiki.Tools[0].Name != "lookup" ||
		wiki.Tools[0].Instances != 2 || wiki.ToolCount != 1 {
		t.Errorf("wiki's listing = %+v, want one tool both instances list", wiki)
	}
}

// fakeListing is one server's listing as a node derives it, with its tools'
// schemas.
func fakeListing(server, node string, epoch int64, tools ...string) nodeListing {
	out := nodeListing{
		listing: coord.MCPListing{Server: server, Node: node, Epoch: epoch, Instances: 1,
			At: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)},
		schemas: map[string]coord.MCPToolSchema{},
	}
	for _, name := range tools {
		digest := testDigest(server + "/" + name)
		out.listing.Tools = append(out.listing.Tools, coord.MCPListedTool{
			Name: name, Raw: name, Description: "does " + name, Digest: digest, SchemaKept: true, Instances: 1,
			Hints: coord.MCPToolHints{ReadOnly: coord.HintYes, Destructive: coord.HintNo,
				Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown},
		})
		out.schemas[digest] = coord.MCPToolSchema{Digest: digest, Input: []byte(`{"type":"object"}`),
			At: out.listing.At}
	}
	return out.bounded(coord.MaxMCPListingSchemaBytes)
}

func testDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// innerFleet names the store a countingFleet wraps.
type innerFleet = coord.Fleet

// countingFleet counts the writes a publisher makes, and refuses the next
// failPuts listing writes as a store that blinked does.
type countingFleet struct {
	innerFleet
	puts, schemas, failPuts atomic.Int32
}

func (c *countingFleet) PutMCPListing(ctx context.Context, l coord.MCPListing) (coord.MCPListing, error) {
	c.puts.Add(1)
	if c.failPuts.Add(-1) >= 0 {
		return coord.MCPListing{}, errors.New("the store did not answer")
	}
	c.failPuts.Store(0)
	return c.innerFleet.PutMCPListing(ctx, l)
}

func (c *countingFleet) CreateMCPToolSchema(ctx context.Context, s coord.MCPToolSchema) (bool, error) {
	c.schemas.Add(1)
	return c.innerFleet.CreateMCPToolSchema(ctx, s)
}

// A LISTING IS WRITTEN WHEN IT MOVES, AND AGAIN UNDER A NEW EPOCH: a quiet
// company writes nothing beat after beat; a listing that changed is written
// with its schemas first; a server the node stops listing is removed; and a
// new presence epoch, which a sweep may have removed everything under the
// last one for, writes everything again.
//
// Mutation: rewrite on every beat, skip the schemas, keep a server the node
// no longer lists, or forget the epoch, and this is red.
func TestAListingIsWrittenWhenItMovesAndAgainUnderANewEpoch(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := &countingFleet{innerFleet: coordmemory.NewFleet()}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	desired := map[string]nodeListing{
		"github": fakeListing("github", "node-a", 3, "get_issue", "create_issue"),
		"docs":   fakeListing("docs", "node-a", 3, "search"),
	}
	e.writeMCPListings(ctx, "node-a", 3, desired)
	if fleet.puts.Load() != 2 || fleet.schemas.Load() != 3 {
		t.Fatalf("the first beat wrote %d listings and %d schemas, want 2 and 3",
			fleet.puts.Load(), fleet.schemas.Load())
	}
	for _, d := range []string{testDigest("github/get_issue"), testDigest("docs/search")} {
		if _, found, err := fleet.MCPToolSchema(ctx, d); err != nil || !found {
			t.Errorf("schema %s is not stored (%v)", d, err)
		}
	}

	e.writeMCPListings(ctx, "node-a", 3, desired)
	if fleet.puts.Load() != 2 || fleet.schemas.Load() != 3 {
		t.Errorf("a beat with nothing moved wrote: %d listings and %d schemas in all",
			fleet.puts.Load(), fleet.schemas.Load())
	}

	// GITHUB MOVES, DOCS GOES.
	moved := map[string]nodeListing{"github": fakeListing("github", "node-a", 3, "get_issue")}
	e.writeMCPListings(ctx, "node-a", 3, moved)
	if fleet.puts.Load() != 3 || fleet.schemas.Load() != 3 {
		t.Errorf("a moved listing wrote %d listings and %d schemas in all, want 3 and 3",
			fleet.puts.Load(), fleet.schemas.Load())
	}
	held, err := fleet.ListMCPListings(ctx)
	if err != nil || len(held) != 1 || held[0].Server != "github" || len(held[0].Tools) != 1 {
		t.Fatalf("the fleet holds %+v (%v), want github's one tool alone", held, err)
	}

	// A NEW EPOCH, as a lapse and a re-claim give: everything again, and
	// the schemas checked again, since a sweep may have taken them.
	e.writeMCPListings(ctx, "node-a", 4, map[string]nodeListing{"github": fakeListing("github", "node-a", 4, "get_issue")})
	if fleet.puts.Load() != 4 || fleet.schemas.Load() != 4 {
		t.Errorf("a new epoch wrote %d listings and %d schemas in all, want 4 and 4",
			fleet.puts.Load(), fleet.schemas.Load())
	}
	held, _ = fleet.ListMCPListings(ctx)
	if len(held) != 1 || held[0].Epoch != 4 {
		t.Errorf("after the new epoch the fleet holds %+v, want github under epoch 4", held)
	}
}

// A SCHEMA IS TRUSTED AS STORED ONLY WHILE A STORED LISTING NAMES IT: when a
// listing gains a tool and its write fails, the store still holds the old
// listing, which does not name the new tool's schemas, so the sweep may remove
// them before the write is tried again; the write that finally lands stores
// them again first rather than naming schemas that are gone.
//
// Mutation: keep every digest the desired listings name as stored, and this
// is red.
func TestASchemaIsStoredAgainWhenTheListingNamingItWasNeverWritten(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := &countingFleet{innerFleet: coordmemory.NewFleet()}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.writeMCPListings(ctx, "node-a", 3, map[string]nodeListing{
		"github": fakeListing("github", "node-a", 3, "get_issue"),
	})
	grown := map[string]nodeListing{"github": fakeListing("github", "node-a", 3, "get_issue", "create_issue")}
	added := testDigest("github/create_issue")

	fleet.failPuts.Store(1)
	e.writeMCPListings(ctx, "node-a", 3, grown)
	held, _ := fleet.ListMCPListings(ctx)
	if len(held) != 1 || len(held[0].Tools) != 1 {
		t.Fatalf("a refused write moved the stored listing: %+v", held)
	}
	// THE SWEEP, which no stored listing gave a reason to keep it.
	schema, found, err := fleet.MCPToolSchema(ctx, added)
	if err != nil || !found {
		t.Fatalf("the new tool's schema was not stored before its listing: %v", err)
	}
	if gone, err := fleet.DeleteMCPToolSchema(ctx, added, schema.Version); err != nil || !gone {
		t.Fatalf("remove the schema: %v %v", gone, err)
	}

	e.writeMCPListings(ctx, "node-a", 3, grown)
	held, _ = fleet.ListMCPListings(ctx)
	if len(held) != 1 || len(held[0].Tools) != 2 {
		t.Fatalf("the retried write left %+v, want github's two tools", held)
	}
	if _, found, err := fleet.MCPToolSchema(ctx, added); err != nil || !found {
		t.Errorf("the listing now names a schema that is not stored (%v)", err)
	}
}

// A LISTING KEEPS ITS SCHEMAS WITHIN ITS BUDGET, IN NAME ORDER: each tool's
// schemas are bounded alone, and a server listing many large ones (or minting
// new definitions at every restart) would otherwise store up to 256 of them per
// listing. Past the budget a tool is listed without its schemas, which are not
// stored; a later tool small enough for what remains is still kept.
//
// Mutation: drop the budget, or keep a tool's flag once it is past it, and
// this is red.
func TestAListingKeepsItsSchemasWithinItsBudgetInNameOrder(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	sized := func(n int) []byte { return []byte(`{"description":"` + strings.Repeat("x", n-18) + `"}`) }
	var names []string
	for i := range 20 {
		names = append(names, fmt.Sprintf("tool_%02d", i))
	}
	names = append(names, "zz_small")
	listing := fakeListing("github", "node-a", 3, names...)
	for _, tool := range listing.listing.Tools {
		size := 60 << 10
		if tool.Name == "zz_small" {
			size = 2 << 10
		}
		schema := listing.schemas[tool.Digest]
		schema.Input = sized(size)
		listing.schemas[tool.Digest] = schema
	}

	got := listing.bounded(coord.MaxMCPListingSchemaBytes)
	var kept []string
	spent := 0
	for _, tool := range got.listing.Tools {
		schema, stored := got.schemas[tool.Digest]
		if tool.SchemaKept != stored {
			t.Errorf("%s is kept = %v with its schema held = %v", tool.Name, tool.SchemaKept, stored)
		}
		if tool.SchemaKept {
			kept = append(kept, tool.Name)
			spent += len(schema.Input) + len(schema.Output)
		}
	}
	// SEVENTEEN OF 60 KIB FIT IN 1 MIB, the eighteenth does not, and the
	// small one after the rest fits what remains.
	want := append(slices.Clone(names[:17]), "zz_small")
	if !slices.Equal(kept, want) || spent > coord.MaxMCPListingSchemaBytes {
		t.Errorf("kept %v (%d bytes), want %v within %d", kept, spent, want, coord.MaxMCPListingSchemaBytes)
	}
	if len(got.listing.Tools) != len(names) {
		t.Errorf("the budget dropped tools from the listing: %d of %d", len(got.listing.Tools), len(names))
	}

	// AND ONLY THOSE ARE STORED.
	fleet := coordmemory.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.writeMCPListings(ctx, "node-a", 3, map[string]nodeListing{"github": got})
	if stored, err := fleet.ListMCPToolSchemas(ctx); err != nil || len(stored) != len(want) {
		t.Errorf("the fleet stores %d schemas (%v), want %d", len(stored), err, len(want))
	}
}

// liveNode puts a node's presence on backend, at the epoch the backend mints.
func liveNode(t *testing.T, backend coord.Backend, node string) int64 {
	t.Helper()
	lease, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
		Owner: node + ":1", TTL: time.Minute,
	})
	if err != nil || lease == nil {
		t.Fatalf("presence of %s: %v", node, err)
	}
	return lease.Epoch
}

// A PER-AGENT SERVER'S TOOLS REACH EVERY NODE'S CATALOGUE THROUGH THE WATCH,
// and a node that drained stops counting: what another node publishes arrives
// on this node's watched copy, counts while its node holds its presence at the
// epoch it was written under, and the surfaces rendering the catalogue are
// told when what they would render moved.
//
// Mutation: count a listing whatever its node's presence, or skip the notice,
// and this is red.
func TestAnotherNodesListingReachesThisNodesCatalogueWhileItsNodeIsLive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	backend := coordmemory.New()
	e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}}
	var notices atomic.Int32
	notify := func(context.Context) { notices.Add(1) }
	e.SetOnCatalogueChanged(notify)

	epochB := liveNode(t, backend, "node-b")
	listing := fakeListing("github", "node-b", epochB, "get_issue")
	if _, err := fleet.PutMCPListing(ctx, listing.listing); err != nil {
		t.Fatal(err)
	}
	for _, s := range listing.schemas {
		if _, err := fleet.CreateMCPToolSchema(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	e.startMCPListings(ctx)
	t.Cleanup(func() { e.listings.cancel(); <-e.listings.done })

	// The publisher's first beat reads the presence; this engine has no
	// node, so it publishes nothing of its own.
	pollUntil(t, 10*time.Second, "node-b's listing counted", func() bool {
		live, ok := e.LiveMCPListings()
		return ok && len(live) == 1 && live[0].Node == "node-b"
	})
	if notices.Load() == 0 {
		t.Error("the catalogue's surfaces were not told of node-b's listing")
	}
	schemas := e.HeldMCPToolSchemas()
	if _, ok := schemas[testDigest("github/get_issue")]; !ok {
		t.Errorf("the schema node-b's listing names was not read: %v", schemas)
	}

	// NODE-B DRAINS: its presence goes, and its listing stops counting at
	// the next reading of the presence, though its record is still there.
	if released, err := backend.Release(ctx, coord.NodeResource("node-b"), "node-b:1", epochB); err != nil || !released {
		t.Fatalf("release node-b: %v %v", released, err)
	}
	before := notices.Load()
	e.publishMCPListings(ctx)
	if live, ok := e.LiveMCPListings(); !ok || len(live) != 0 {
		t.Errorf("a drained node's listing still counts: %+v", live)
	}
	if notices.Load() == before {
		t.Error("the catalogue's surfaces were not told that a drained node's tools went")
	}
}

// THE SWEEP REMOVES WHAT READERS NO LONGER COUNT, AND NOTHING ELSE: a listing
// whose node has no presence or another incarnation goes; a live one stays; a
// schema goes only once two sweeps in a row found no listing naming it, since
// a node stores a schema before the listing that names it.
//
// Mutation: sweep a schema on its first unnamed pass, or keep a listing of a
// node that claimed again at a new epoch, and this is red.
func TestTheSweepRemovesOnlyWhatNoReaderCounts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	backend := coordmemory.New()
	e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}}
	epochA := liveNode(t, backend, "node-a")
	put := func(l nodeListing) {
		t.Helper()
		if _, err := fleet.PutMCPListing(ctx, l.listing); err != nil {
			t.Fatal(err)
		}
		for _, s := range l.schemas {
			if _, err := fleet.CreateMCPToolSchema(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(fakeListing("github", "node-a", epochA, "get_issue"))
	put(fakeListing("github", "node-gone", 9, "list_issues"))
	put(fakeListing("docs", "node-a", epochA+1, "search"))

	removed, err := e.sweepMCPListings(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("the first sweep removed %d (%v), want the dead node's listing and the stale epoch's", removed, err)
	}
	held, _ := fleet.ListMCPListings(ctx)
	if len(held) != 1 || held[0].Node != "node-a" || held[0].Server != "github" {
		t.Fatalf("after the sweep the fleet holds %+v, want node-a's github listing alone", held)
	}
	if schemas, _ := fleet.ListMCPToolSchemas(ctx); len(schemas) != 3 {
		t.Fatalf("the first sweep left %d schemas, want all three until a second sweep agrees", len(schemas))
	}
	removed, err = e.sweepMCPListings(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("the second sweep removed %d (%v), want the two schemas nothing names", removed, err)
	}
	schemas, _ := fleet.ListMCPToolSchemas(ctx)
	if len(schemas) != 1 || schemas[0].Digest != testDigest("github/get_issue") {
		t.Errorf("after two sweeps the schemas are %+v, want get_issue's alone", schemas)
	}
}

// A DRAINED NODE'S LISTING GOES: the node removes its own listings when it
// stops, rather than leaving them for the sweep.
//
// Mutation: stop the publisher without removing what it wrote, and this is
// red.
func TestANodeThatStopsRemovesItsOwnListings(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet, Coord: coordmemory.New()}, testNode: "node-a"}
	e.startMCPListings(ctx)
	e.writeMCPListings(ctx, "node-a", 3, map[string]nodeListing{
		"github": fakeListing("github", "node-a", 3, "get_issue"),
	})
	if held, _ := fleet.ListMCPListings(ctx); len(held) != 1 {
		t.Fatalf("the fleet holds %+v, want node-a's listing", held)
	}
	e.stopMCPListings(ctx)
	if held, _ := fleet.ListMCPListings(ctx); len(held) != 0 {
		t.Errorf("a stopped node left %+v", held)
	}
}
