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
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/seat/placement"
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
		if tool.Instances != 1 || !coord.IsMCPDigest(tool.Digest) || !tool.DefinitionKept {
			t.Errorf("tracker's %s = %+v, want one instance listing it and its schema kept", tool.Name, tool)
		}
		if _, ok := listings["tracker"].definitions[tool.Digest]; !ok {
			t.Errorf("tracker's %s names a schema its listing does not carry", tool.Name)
		}
	}
	if !slices.Equal(names, []string{"probe_1", "probe_2"}) {
		t.Errorf("tracker lists %v, want both children's tools", names)
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
		wiki.Tools[0].Instances != 2 {
		t.Errorf("wiki's listing = %+v, want one tool both instances list", wiki)
	}
}

// fakeListing is one server's listing as a node derives it, with its tools'
// schemas.
func fakeListing(server, node string, epoch int64, tools ...string) nodeListing {
	out := nodeListing{
		listing: coord.MCPListing{Server: server, Node: node, Epoch: epoch, Instances: 1,
			At: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)},
		definitions: map[string]coord.MCPToolDefinition{},
	}
	for _, name := range tools {
		digest := testDigest(server + "/" + name)
		hints := coord.MCPToolHints{ReadOnly: coord.HintYes, Destructive: coord.HintNo,
			Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown}
		part := digest[:coord.MCPToolPartLength]
		out.listing.Tools = append(out.listing.Tools, coord.MCPListedTool{
			Name: name, Raw: name, Description: "does " + name, Digest: digest, DefinitionKept: true, Instances: 1,
			Hints: hints, Declared: hints,
			Parts: coord.MCPToolParts{Title: part, Description: part, Schema: part, Hints: part},
		})
		out.definitions[digest] = coord.MCPToolDefinition{Digest: digest, Input: []byte(`{"type":"object"}`),
			At: out.listing.At}
	}
	return out.bounded(coord.MaxMCPListingDefinitionBytes, nil)
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
	puts, schemas, failPuts, reads atomic.Int32
}

func (c *countingFleet) MCPToolDefinition(ctx context.Context, digest string) (coord.MCPToolDefinition, bool, error) {
	c.reads.Add(1)
	return c.innerFleet.MCPToolDefinition(ctx, digest)
}

func (c *countingFleet) PutMCPListing(ctx context.Context, l coord.MCPListing) (coord.MCPListing, error) {
	c.puts.Add(1)
	if c.failPuts.Add(-1) >= 0 {
		return coord.MCPListing{}, errors.New("the store did not answer")
	}
	c.failPuts.Store(0)
	return c.innerFleet.PutMCPListing(ctx, l)
}

func (c *countingFleet) CreateMCPToolDefinition(ctx context.Context, s coord.MCPToolDefinition) (bool, error) {
	c.schemas.Add(1)
	return c.innerFleet.CreateMCPToolDefinition(ctx, s)
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
		if _, found, err := fleet.MCPToolDefinition(ctx, d); err != nil || !found {
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

// A DEFINITION IS TRUSTED AS STORED ONLY WHILE A STORED LISTING NAMES IT: when
// a listing gains a tool and its write fails, the store still holds the old
// listing, which does not name the new tool's definition, so the sweep may
// remove it before the write is tried again; the write that finally lands
// stores it again first rather than naming a definition that is gone.
//
// Mutation: keep every digest the desired listings name as stored, and this
// is red.
func TestADefinitionIsStoredAgainWhenTheListingNamingItWasNeverWritten(t *testing.T) {
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
	schema, found, err := fleet.MCPToolDefinition(ctx, added)
	if err != nil || !found {
		t.Fatalf("the new tool's schema was not stored before its listing: %v", err)
	}
	if gone, err := fleet.DeleteMCPToolDefinition(ctx, added, schema.Version); err != nil || !gone {
		t.Fatalf("remove the schema: %v %v", gone, err)
	}

	e.writeMCPListings(ctx, "node-a", 3, grown)
	held, _ = fleet.ListMCPListings(ctx)
	if len(held) != 1 || len(held[0].Tools) != 2 {
		t.Fatalf("the retried write left %+v, want github's two tools", held)
	}
	if _, found, err := fleet.MCPToolDefinition(ctx, added); err != nil || !found {
		t.Errorf("the listing now names a schema that is not stored (%v)", err)
	}
}

// A LISTING KEEPS ITS DEFINITIONS WITHIN ITS BUDGET, A CHANGE WAITING FOR A
// REVIEW FIRST, THEN IN NAME ORDER: each definition is bounded alone, and a
// server listing many large ones (or minting new ones at every restart) would
// otherwise store up to 256 of them per listing. Its words weigh as its schemas
// do. Past the budget a tool is listed without its definition, which is not
// stored; a later tool small enough for what remains is still kept; and a tool
// whose definition waits for a review is kept before any other, so the change
// a person is asked about can be shown whole.
//
// Mutation: drop the budget, weigh the schemas alone, keep a tool's flag once
// it is past it, or keep in name order alone, and this is red.
func TestAListingKeepsItsDefinitionsWithinItsBudgetAChangeFirst(t *testing.T) {
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
		schema, words := 40<<10, 20<<10
		if tool.Name == "zz_small" {
			schema, words = 1<<10, 1<<10
		}
		definition := listing.definitions[tool.Digest]
		definition.Input, definition.Description = sized(schema), strings.Repeat("d", words)
		listing.definitions[tool.Digest] = definition
	}

	waiting := func(tool, _ string) bool { return tool == "tool_19" }
	got := listing.bounded(coord.MaxMCPListingDefinitionBytes, waiting)
	var kept []string
	spent := 0
	for _, tool := range got.listing.Tools {
		definition, stored := got.definitions[tool.Digest]
		if tool.DefinitionKept != stored {
			t.Errorf("%s is kept = %v with its definition held = %v", tool.Name, tool.DefinitionKept, stored)
		}
		if tool.DefinitionKept {
			kept = append(kept, tool.Name)
			spent += definition.Size()
		}
	}
	// SEVENTEEN OF 60 KIB FIT IN 1 MIB: the one waiting for a review, then
	// sixteen in name order, and the small one after the rest fits what
	// remains.
	want := append(slices.Clone(names[:16]), "tool_19", "zz_small")
	if !slices.Equal(kept, want) || spent > coord.MaxMCPListingDefinitionBytes {
		t.Errorf("kept %v (%d bytes), want %v within %d", kept, spent, want, coord.MaxMCPListingDefinitionBytes)
	}
	if len(got.listing.Tools) != len(names) {
		t.Errorf("the budget dropped tools from the listing: %d of %d", len(got.listing.Tools), len(names))
	}

	// AND ONLY THOSE ARE STORED.
	fleet := coordmemory.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.writeMCPListings(ctx, "node-a", 3, map[string]nodeListing{"github": got})
	if stored, err := fleet.ListMCPToolDefinitions(ctx); err != nil || len(stored) != len(want) {
		t.Errorf("the fleet stores %d definitions (%v), want %d", len(stored), err, len(want))
	}
}

// A DEFINITION THE STORE DOES NOT HOLD IS READ ONCE, NOT ON EVERY READ OF A
// SERVER'S PAGE: the miss is remembered until a listing naming the digest as
// kept arrives, since a node stores a definition before the listing that says
// so; then it is read again and found.
//
// Mutation: read every missing digest from the store each time, or keep the
// miss after a listing says it was kept, and this is red.
func TestADefinitionTheStoreDoesNotHoldIsReadOnceUntilAListingKeepsIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	inner := coordmemory.NewFleet()
	fleet := &countingFleet{innerFleet: inner}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	unkept := fakeListing("github", "node-a", 3, "get_issue")
	digest := unkept.listing.Tools[0].Digest
	unkept.listing.Tools[0].DefinitionKept = false
	e.applyMCPListing(ctx, coord.MCPListingUpdate{Listing: &unkept.listing, Server: "github", Node: "node-a"})
	for range 3 {
		if got, err := e.MCPToolDefinitions(ctx, []string{digest}); err != nil || len(got) != 0 {
			t.Fatalf("a definition the store does not hold read as %+v (%v)", got, err)
		}
	}
	if n := fleet.reads.Load(); n != 1 {
		t.Fatalf("three reads of a missing definition read the store %d times, want once", n)
	}

	// ANOTHER NODE STORES IT, then its listing says so.
	if _, err := inner.CreateMCPToolDefinition(ctx, unkept.definitions[digest]); err != nil {
		t.Fatal(err)
	}
	kept := fakeListing("github", "node-b", 2, "get_issue")
	e.applyMCPListing(ctx, coord.MCPListingUpdate{Listing: &kept.listing, Server: "github", Node: "node-b"})
	got, err := e.MCPToolDefinitions(ctx, []string{digest})
	if err != nil || len(got) != 1 || fleet.reads.Load() != 2 {
		t.Fatalf("after a listing kept it the definition read as %+v (%v) in %d reads, want it found",
			got, err, fleet.reads.Load())
	}
}

// THE MOST TOOLS A SERVER MAY LIST FIT ONE LISTING, AT THEIR LONGEST: every
// server the client takes is listed whole, never refused by the listing's
// bytes, whatever its words. Names are of the characters a tool is filed
// under, at the longest a listing keeps.
//
// Mutation: raise mcp.MaxServerTools past what MaxMCPListingBytes holds, and
// this is red.
func TestTheMostToolsAServerListsFitOneListing(t *testing.T) {
	t.Parallel()
	unknown := coord.MCPToolHints{ReadOnly: coord.HintUnknown, Destructive: coord.HintUnknown,
		Idempotent: coord.HintUnknown, OpenWorld: coord.HintUnknown}
	l := coord.MCPListing{Server: "cloud", Node: "node-a", Epoch: 1, At: time.Now(), Instances: 9999}
	for i := range mcp.MaxServerTools {
		name := fmt.Sprintf("%0*d", coord.MaxMCPToolNameBytes, i)
		part := testDigest(name)[:coord.MCPToolPartLength]
		l.Tools = append(l.Tools, coord.MCPListedTool{Name: name, Raw: name,
			Title: strings.Repeat("t", coord.MaxMCPToolTitleBytes), Description: strings.Repeat("d",
				coord.MaxMCPToolDescriptionBytes), Hints: unknown, Declared: unknown, Digest: testDigest(name),
			DefinitionKept: true, Instances: 9999, Blocked: true,
			Parts: coord.MCPToolParts{Title: part, Description: part, Schema: part, Hints: part}})
	}
	bounded := l.Bounded()
	if err := bounded.Validate(); err != nil {
		t.Fatalf("a listing of the most tools a server may list is refused: %v", err)
	}
	if len(bounded.Tools) != mcp.MaxServerTools || bounded.EncodedSize() > coord.MaxMCPListingBytes {
		t.Errorf("the listing names %d tools in %d bytes, want %d within %d", len(bounded.Tools),
			bounded.EncodedSize(), mcp.MaxServerTools, coord.MaxMCPListingBytes)
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
// What is held is what a surface reads WHEN IT IS TOLD. The notice follows the
// state it announces, on the goroutine that stored it, so that state can be
// read a moment before any surface hears of it; the promise is only that a
// surface told reads what it was told of.
//
// Mutation: count a listing whatever its node's presence, skip the notice, or
// send it before the schemas the listing names are read, and this is red.
func TestAnotherNodesListingReachesThisNodesCatalogueWhileItsNodeIsLive(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := coordmemory.NewFleet()
	backend := coordmemory.New()
	e := &Engine{backends: &Backends{Fleet: fleet, Coord: backend}}
	var (
		mu       sync.Mutex
		readings []catalogueReading
	)
	e.SetOnCatalogueChanged(func(context.Context) {
		live, _ := e.LiveMCPListings()
		r := catalogueReading{live: live, schemas: e.HeldMCPToolDefinitions()}
		mu.Lock()
		readings = append(readings, r)
		mu.Unlock()
	})
	told := func() []catalogueReading {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(readings)
	}

	epochB := liveNode(t, backend, "node-b")
	listing := fakeListing("github", "node-b", epochB, "get_issue")
	if _, err := fleet.PutMCPListing(ctx, listing.listing); err != nil {
		t.Fatal(err)
	}
	for _, s := range listing.definitions {
		if _, err := fleet.CreateMCPToolDefinition(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	e.startMCPListings(ctx)
	t.Cleanup(func() { e.listings.cancel(); <-e.listings.done })

	// The publisher's first beat reads the presence; this engine has no
	// node, so it publishes nothing of its own.
	pollUntil(t, 10*time.Second, "the catalogue's surfaces told of node-b's listing", func() bool {
		return len(told()) > 0
	})
	first := told()[0]
	if len(first.live) != 1 || first.live[0].Node != "node-b" {
		t.Errorf("a surface told of the catalogue read %+v, want node-b's listing", first.live)
	}
	if _, ok := first.schemas[testDigest("github/get_issue")]; !ok {
		t.Errorf("a surface told of the catalogue could not read the schema node-b's listing names: %v",
			first.schemas)
	}

	// ONE BEAT AT A TIME, as a node runs them on its publisher's goroutine
	// alone: the loop stops, and the case's beat stands in for its next one.
	// Left running, a tick between the release and the case's beat would
	// announce the drain before the count below is taken, and two beats
	// reading the presence either side of the release could store their
	// readings in the opposite order.
	e.listings.cancel()
	<-e.listings.done

	// NODE-B DRAINS: its presence goes, and its listing stops counting at
	// the next reading of the presence, though its record is still there.
	if released, err := backend.Release(ctx, coord.NodeResource("node-b"), "node-b:1", epochB); err != nil || !released {
		t.Fatalf("release node-b: %v %v", released, err)
	}
	before := len(told())
	e.publishMCPListings(ctx)
	if live, ok := e.LiveMCPListings(); !ok || len(live) != 0 {
		t.Errorf("a drained node's listing still counts: %+v", live)
	}
	after := told()
	if len(after) == before {
		t.Fatal("the catalogue's surfaces were not told that a drained node's tools went")
	}
	if last := after[len(after)-1]; len(last.live) != 0 {
		t.Errorf("a surface told that node-b drained read %+v, want no listing", last.live)
	}
}

// catalogueReading is what a surface rendering the catalogue reads when it is
// told the catalogue moved.
type catalogueReading struct {
	live    []coord.MCPListing
	schemas map[string]coord.MCPToolDefinition
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
		for _, s := range l.definitions {
			if _, err := fleet.CreateMCPToolDefinition(ctx, s); err != nil {
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
	if schemas, _ := fleet.ListMCPToolDefinitions(ctx); len(schemas) != 3 {
		t.Fatalf("the first sweep left %d schemas, want all three until a second sweep agrees", len(schemas))
	}
	removed, err = e.sweepMCPListings(ctx)
	if err != nil || removed != 2 {
		t.Fatalf("the second sweep removed %d (%v), want the two schemas nothing names", removed, err)
	}
	schemas, _ := fleet.ListMCPToolDefinitions(ctx)
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

// heldRemovals holds every listing removal until the case releases it, handing
// the case the context each was made in: a store whose link carries a pass's
// bytes slowly, the removal standing in for every write a pass makes.
type heldRemovals struct {
	innerFleet
	made     chan context.Context
	release  chan struct{}
	removals atomic.Int32
}

func (h *heldRemovals) DeleteMCPListing(ctx context.Context, server, node string, version uint64) (bool, error) {
	h.removals.Add(1)
	h.made <- ctx
	select {
	case <-h.release:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	return h.innerFleet.DeleteMCPListing(ctx, server, node, version)
}

// A SLOW LISTING WRITE HOLDS NO BEAT, AND WAITS AS LONG AS ITS BYTES TAKE.
//
// A beat's listing writes are made on a pass of their own, in the publisher's
// own context, with no deadline of the beat's: each waits as long as the
// coordination store's rule gives the bytes it carries, which for a listing at
// its bound across a link at the floor is past the beat's fifteen seconds. And
// one pass at a time: a beat that finds a pass in flight leaves its listings
// to it. Here a node holding its presence has one listing its last beat wrote
// to remove, and the store holds the removal until the case releases it: the
// beat returns with the write in flight on its pass, carrying no deadline of
// the beat's; the next beat returns too, and the store is asked for the
// removal once, by the pass in flight; and once released, that pass removes
// the listing.
//
// THE BEATS RUN IN A BUBBLE, the node they read outside it: a pass a beat
// starts is a goroutine of the bubble, so once every goroutine in it is
// blocked (synctest.Wait), a pass the next beat started has reached the store
// with its removal. Outside a bubble such a pass could still be starting when
// the case released the first, and read the listing as already removed, so a
// beat that started one could pass.
//
// Mutation: hold the pass's writes to the beat's fifteen seconds, and the write
// carries the beat's deadline; start a pass on every beat, and the next beat's
// pass asks the store for the removal a second time while the first is held.
func TestASlowListingWriteHoldsNoBeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	backend := coordmemory.New()
	n, err := node.New(node.Config{
		Queue: memory.New(), Coord: backend, NodeID: "node-a", Owner: "node-a:1",
		Seats: func() []placement.Seat { return nil },
		Turn: func(context.Context, string, []*events.Event) queue.Result {
			return queue.Ack()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Start(ctx); err != nil {
		t.Fatalf("start the node: %v", err)
	}
	t.Cleanup(func() { n.Stop(context.WithoutCancel(ctx)) })
	epoch, held := n.Host().PresenceEpoch()
	if !held {
		t.Fatal("a started node holds no presence")
	}
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		fleet := &heldRemovals{innerFleet: coordmemory.NewFleet(), made: make(chan context.Context, 2),
			release: make(chan struct{})}
		stored, err := fleet.PutMCPListing(ctx, fakeListing("gone", "node-a", epoch, "get_issue").listing)
		if err != nil {
			t.Fatalf("the listing the last beat wrote: %v", err)
		}
		e := &Engine{node: n, backends: &Backends{Fleet: fleet, Coord: backend}}
		e.listings.epoch, e.listings.ensured = epoch, map[string]struct{}{}
		e.listings.published = map[string]publishedListing{"gone": {fingerprint: "listed", version: stored.Version}}

		e.publishMCPListings(ctx)
		if _, set := (<-fleet.made).Deadline(); set {
			t.Error("the pass's write carries a deadline of the beat's, which a listing at its " +
				"bound across a link at the floor outlasts")
		}
		// THE NEXT BEAT, with the write still held: it returns, its
		// listings left to the pass in flight, and once every goroutine is
		// blocked the store has been asked for the removal by that pass alone.
		e.publishMCPListings(ctx)
		synctest.Wait()
		if got := fleet.removals.Load(); got != 1 {
			t.Errorf("with the first removal held, the store was asked for it %d times, want "+
				"once: a beat started a pass beside the one in flight", got)
		}
		close(fleet.release)
		e.listings.pass.Wait()
		if left, err := fleet.ListMCPListings(ctx); err != nil || len(left) != 0 {
			t.Errorf("after the pass the store holds %+v (%v), want the listing removed", left, err)
		}
	})
}
