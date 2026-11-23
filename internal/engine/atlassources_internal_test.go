package engine

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
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

// missesRefused is a fleet whose read of a source's misses fails, counting
// each time it is asked.
type missesRefused struct {
	innerFleet
	asked atomic.Int32
}

func (f *missesRefused) AtlasSourceMisses(context.Context, string) ([]byte, bool, error) {
	f.asked.Add(1)
	return nil, false, errors.New("the misses crossed no link in time")
}

// A REPORT OF THE SOURCES READS THEIR STANDINGS ALONE: what a source stopped
// seeing is the standing's count, and its misses, a record of up to 768 KiB a
// source, are never asked by a person's question, a dashboard's poll or the
// atlas_refresh tool.
//
// Mutation: have the report read each source's misses to count them, and it
// fails here.
func TestAnAtlasSourceReportReadsNoSourcesMisses(t *testing.T) {
	t.Parallel()
	fleet := &missesRefused{innerFleet: coordmemory.NewFleet()}
	cfg := &config.Company{Name: "acme"}
	cfg.Integrations.Cloudflare = &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
		AccountID: "0123456789abcdef0123456789abcdef", APIToken: "${CF_TOKEN}"}}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.epoch.current.Store(&Company{Config: cfg})
	store, err := e.AtlasSourceStore()
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	if err := store.Save(t.Context(), collect.Status{Kind: integration.KindCloudflare, LastAttemptAt: at,
		Completed: true, LastCompleteAt: at, Missed: 3}); err != nil {
		t.Fatal(err)
	}
	report, running, err := e.AtlasSourceReport(t.Context(), at.Add(time.Minute))
	if err != nil || !running {
		t.Fatalf("the report = %v, %v", running, err)
	}
	i := slices.IndexFunc(report.Sources, func(s collect.Standing) bool { return s.Kind == integration.KindCloudflare })
	if i < 0 || report.Sources[i].Missing != 3 {
		t.Errorf("the report = %+v, want cloudflare missing its three", report.Sources)
	}
	if n := fleet.asked.Load(); n != 0 {
		t.Errorf("the report asked the store for a source's misses %d times, want none", n)
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
