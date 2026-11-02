package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// LISTING A RUNNING SERVER AGAIN (relist.go): a server that changes a tool
// while it runs is seen to, on its own schedule held to the engine's bounds
// and whenever it says its list changed, and never from a copy the SDK kept.

// THE BOUNDS ARE THE ENGINE'S: a server stating a moment is listed a floor
// later, one stating a day a ceiling later, and one stating nothing at the
// ceiling, so no server makes the engine list it constantly or opts out of
// being watched.
func TestARunningServerIsListedAgainOnItsTTLHeldBetweenTheFloorAndTheCeiling(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		ttl    time.Duration
		stated bool
		want   time.Duration
	}{
		{"none stated", 0, false, RelistCeiling},
		{"zero, immediately stale", 0, true, RelistFloor},
		{"under the floor", 5 * time.Second, true, RelistFloor},
		{"between the bounds", 7 * time.Minute, true, 7 * time.Minute},
		{"past the ceiling", 24 * time.Hour, true, RelistCeiling},
	} {
		if got := RelistAfter(c.ttl, c.stated, RelistFloor); got != c.want {
			t.Errorf("%s: listed again after %s, want %s", c.name, got, c.want)
		}
	}
	if RelistFloor != time.Minute || RelistCeiling != 15*time.Minute {
		t.Errorf("the bounds are %s and %s; ADR-0078 and relist.go justify a minute and a quarter "+
			"of an hour", RelistFloor, RelistCeiling)
	}
}

// writeTools makes the helper server behind path list tools from now on.
func writeTools(t *testing.T, path string, tools ...[3]string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(toolsJSON(tools...)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A TOOL THE SERVER CHANGES WHILE IT RUNS IS REFILED: the session is listed
// again when the TTL it stated runs out, the tool comes back under its new
// definition (a new digest), and the owner is told the catalogue changed,
// with no restart.
func TestAToolAServerChangesWhileItRunsIsRefiledWhenItsTTLRunsOut(t *testing.T) {
	t.Parallel()
	changed := make(chan struct{}, 8)
	b, rec := supervisedBridge(t, Supervision{RelistBase: 50 * time.Millisecond,
		CatalogueChanged: func(context.Context) { changed <- struct{}{} }})
	tools := filepath.Join(t.TempDir(), "tools.json")
	writeTools(t, tools, [3]string{"search", "Search the wiki", `{"readOnlyHint":true}`})
	spec, starts := countedSpec(t, "wiki", "serve", map[string]string{
		helperToolsFileEnv: tools, helperTTLEnv: "100"})
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	before, ok := b.Tool("search")
	if !ok {
		t.Fatal("search was not filed")
	}
	writeTools(t, tools, [3]string{"search", "Search the wiki and delete what it finds",
		`{"readOnlyHint":false,"destructiveHint":true}`})
	select {
	case <-changed:
	case <-time.After(30 * time.Second):
		t.Fatal("the owner was never told the server changed a tool")
	}
	after, ok := b.Tool("search")
	if !ok || after.Digest() == before.Digest() || after.Description() != "Search the wiki and delete what it finds" {
		t.Fatalf("after the change search is %+v (%v), want its new definition", after, ok)
	}
	if after.Annotations().Destructive != Yes {
		t.Error("the changed hints were not read")
	}
	if n := startsIn(starts); n != 1 {
		t.Errorf("the server was started %d times: listing it again must not restart it", n)
	}
	if !rec.has("server_catalogue_changed") {
		t.Error("the change was not logged")
	}
}

// A NOTIFICATION LISTS SOONER, NEVER BEFORE THE FLOOR: a server that says its
// list changed is listed again well before its TTL, and one that says so on
// every call is listed no more than once a floor however often it says it.
func TestAListChangedNotificationListsAgainNoSoonerThanTheFloor(t *testing.T) {
	t.Parallel()
	const floor = 300 * time.Millisecond
	b, _ := supervisedBridge(t, Supervision{RelistBase: floor})
	dir := t.TempDir()
	tools, lists := filepath.Join(dir, "tools.json"), filepath.Join(dir, "lists")
	writeTools(t, tools, [3]string{"search", "Search", ""})
	// A DAY'S TTL, held to the ceiling of fifteen floors: only the
	// notification can bring a listing in under it.
	spec, _ := countedSpec(t, "chatty", "serve", map[string]string{
		helperToolsFileEnv: tools, helperTTLEnv: "86400000", helperNotifyEnv: "1", helperListsEnv: lists})
	if _, err := b.Add(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	writeTools(t, tools, [3]string{"search", "Search, reworded", ""})
	tool, _ := b.Tool("search")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := tool.Call(t.Context(), map[string]any{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if now, _ := b.Tool("search"); now.Description() != "Search, reworded" {
		t.Fatalf("after notifications for %s search still reads %q: the notification listed nothing",
			time.Since(started).Round(time.Millisecond), now.Description())
	}
	// ONE LISTING AT START, then at most one a floor: a hundred
	// notifications in two seconds are worth about seven listings.
	elapsed := time.Since(started)
	if n, most := startsIn(lists), 2+int(elapsed/floor); n > most {
		t.Errorf("the server was listed %d times in %s of notifications, more than once a floor (%d)",
			n, elapsed.Round(time.Millisecond), most)
	}
}

// THE SDK NEVER ANSWERS A LISTING FROM ITS COPY: the probe reads the TTL a
// server states and hands the SDK the listing with no TTL at all, so a server
// stating a day is still asked when the engine lists it again.
func TestTheProbeHandsTheSDKAListingThatIsNeverFresh(t *testing.T) {
	t.Parallel()
	c := &probeConn{hints: newHintTable()}
	c.hints.beginListing()
	in := json.RawMessage(`{"tools":[{"name":"search","annotations":{"readOnlyHint":true}}],"ttlMs":86400000}`)
	out := c.recordListing(in)
	var got struct {
		TTLMs int             `json:"ttlMs"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the SDK was handed %s: %v", out, err)
	}
	if got.TTLMs != 0 || len(got.Tools) == 0 {
		t.Errorf("the SDK was handed %s, want the same tools and ttlMs 0", out)
	}
	if ttl, stated := c.hints.listingTTL(); !stated || ttl != 24*time.Hour {
		t.Errorf("the hint read is %s (stated %v), want a day", ttl, stated)
	}
	if ann, ok := c.hints.lookup("search"); !ok || ann.ReadOnly != Yes {
		t.Error("the hints were not read from the listing whose TTL was rewritten")
	}
	c.hints.beginListing()
	plain := json.RawMessage(`{"tools":[]}`)
	if out := c.recordListing(plain); string(out) != string(plain) {
		t.Errorf("a listing stating no TTL was handed on as %s", out)
	}
	if _, stated := c.hints.listingTTL(); stated {
		t.Error("a listing stating no TTL was read as stating one")
	}
}
