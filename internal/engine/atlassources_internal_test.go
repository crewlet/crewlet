package engine

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
)

// The collect duty reads every registered vendor and, once, the SSH source
// the engine adds beside them. Mutation: hand the duty the registry alone,
// and no server that opts in is ever read for the atlas.
func TestTheCollectDutyReadsTheServersBesideEveryVendor(t *testing.T) {
	t.Parallel()
	var kinds []integration.Kind
	for _, s := range (&Engine{}).atlasSources() {
		kinds = append(kinds, s.Kind())
	}
	if n := len(kinds); n != len(collect.Registered())+1 || kinds[n-1] != integration.KindSSH {
		t.Fatalf("the duty reads %v, want every registered vendor and then SSH", kinds)
	}
	for _, s := range collect.Registered() {
		if s.Kind() == integration.KindSSH {
			t.Fatal("an SSH source is registered too, so the duty would refuse two of one kind")
		}
		if !slices.Contains(kinds, s.Kind()) {
			t.Errorf("the duty does not read %s", s.Kind())
		}
	}
}

// storedListingsRefused is a fleet whose listing of the MCP listings fails,
// counting each time it is asked.
type storedListingsRefused struct {
	innerFleet
	asked atomic.Int32
}

func (f *storedListingsRefused) ListMCPListings(context.Context) ([]coord.MCPListing, error) {
	f.asked.Add(1)
	return nil, errors.New("the store's listing crossed no link in time")
}

// THE ATLAS READS THE FLEET'S LISTINGS FROM THE WATCHED COPY: a turn's atlas
// read listing the store would carry every node's listing, each up to 896 KiB,
// across a leaf's link while the turn waits; the watch already holds them.
//
// Mutation: hand the atlas the store's listings, and they are asked of it.
func TestTheAtlasReadsTheFleetsListingsFromTheWatchedCopy(t *testing.T) {
	t.Parallel()
	fleet := &storedListingsRefused{innerFleet: coordmemory.NewFleet()}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	listing := fakeListing("github", "node-b", 1, "get_issue").listing
	e.listings.fleet = map[string]coord.MCPListing{coord.MCPListingKey("github", "node-b"): listing}
	e.listings.answered = true

	src := e.atlasRecords(t.Context())
	if src.Listings == nil {
		t.Fatal("the atlas is handed no listings")
	}
	got, err := src.Listings.MCPListings(t.Context())
	if err != nil || len(got) != 1 || got[0].Server != "github" || got[0].Node != "node-b" {
		t.Fatalf("the atlas reads the listings %+v (%v), want node-b's watched listing", got, err)
	}
	if n := fleet.asked.Load(); n != 0 {
		t.Errorf("the atlas asked the store for the listings %d times, want none", n)
	}

	// UNREAD UNTIL THE WATCH ANSWERS, never empty.
	e.listings.answered = false
	if got, err := src.Listings.MCPListings(t.Context()); err == nil {
		t.Errorf("before the watch answered the atlas reads %+v, want an error it reports as unread", got)
	}
}
