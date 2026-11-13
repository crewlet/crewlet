package builtin_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/tools"
)

// refreshDesk is the atlas's sources over the memory coordination store, for
// a company that connects Cloudflare and Datadog.
type refreshDesk struct {
	store *collect.CoordStore
	asks  int
}

func (d *refreshDesk) AtlasSourceReport(ctx context.Context, now time.Time) (collect.Report, bool, error) {
	standings, err := d.store.Load(ctx)
	if err != nil {
		return collect.Report{}, false, err
	}
	asked, err := d.store.Requests(ctx)
	if err != nil {
		return collect.Report{}, false, err
	}
	return collect.Describe([]integration.Kind{integration.KindCloudflare, integration.KindDatadog},
		standings, asked, time.Hour, now), true, nil
}

func (d *refreshDesk) AskAtlasRefresh(ctx context.Context, kind integration.Kind, req collect.Request) error {
	d.asks++
	return d.store.Ask(ctx, kind, req)
}

// stillSources is the sources of a company that connects Cloudflare, never
// read and asked for by nobody, for a catalogue that only has to serve the
// tool: an ask is taken and kept nowhere.
type stillSources struct{}

func (stillSources) AtlasSourceReport(_ context.Context, now time.Time) (collect.Report, bool, error) {
	return collect.Describe([]integration.Kind{integration.KindCloudflare}, nil, nil, time.Hour, now), true, nil
}

func (stillSources) AskAtlasRefresh(context.Context, integration.Kind, collect.Request) error {
	return nil
}

var refreshNow = time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)

// refreshRig is the operator catalogue's atlas_refresh over desk, decided by
// the real authority table.
func refreshRig(t *testing.T) (*refreshDesk, tools.Callable) {
	t.Helper()
	store, err := collect.NewCoordStore(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	desk := &refreshDesk{store: store}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		AtlasRefresh: builtin.AtlasRefreshDeps{Sources: desk, Actor: builtin.PrincipalActor,
			Now: func() time.Time { return refreshNow }},
		Authorize: builtin.Decide(seatLeads),
	}) {
		if tool.Name() == builtin.AtlasRefreshTool {
			return desk, tool
		}
	}
	t.Fatal("the operator catalogue serves no atlas_refresh with a desk wired")
	return nil, nil
}

func refreshAs(p iam.Principal) context.Context {
	return builtin.OutsideAnyTurn(iam.WithPrincipal(context.Background(), p))
}

func callRefresh(t *testing.T, tool tools.Callable, ctx context.Context, source string) (tools.Result, map[string]any) {
	t.Helper()
	res, err := tool.Call(ctx, map[string]any{"integration": source})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("answered %q, not JSON: %v", res.Output, err)
		}
	}
	return res, out
}

// AN ASK IS RECORDED ONCE, IN THE ASKER'S NAME, AND A SECOND ASK IS NONE: the
// first answers that it changed something and leaves the ask the duty reads,
// and asking again answers with that ask and writes nothing.
//
// Mutation: record on every call, and the desk takes two asks.
func TestARefreshIsAskedOnceInTheAskersName(t *testing.T) {
	t.Parallel()
	desk, tool := refreshRig(t)
	ada := refreshAs(boundTo("ada", iam.GrantAtlas))
	res, out := callRefresh(t, tool, ada, "Cloudflare")
	if res.Failed || out["changed"] != true || out["requested_by"] != "ada" {
		t.Fatalf("the first ask answered %q", res.Output)
	}
	asked, err := desk.store.Requests(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := asked[integration.KindCloudflare]; !got.RequestedAt.Equal(refreshNow) || got.By != "ada" {
		t.Fatalf("the ask on record is %+v", got)
	}
	res, out = callRefresh(t, tool, refreshAs(boundTo("jane", iam.GrantAtlas)), "cloudflare")
	if res.Failed || out["changed"] != false || out["requested_by"] != "ada" || desk.asks != 1 {
		t.Errorf("a second ask answered %q after %d writes, want the first ask and one write", res.Output, desk.asks)
	}
}

// THE FLOOR IS THE ENGINE'S: a source read less than the floor ago is refused
// with the instant it can be asked from, and nothing is recorded; one read
// longer ago is asked.
//
// Mutation: drop the floor check, and the ask a minute after a read is taken.
func TestARefreshInsideTheFloorIsRefusedWithItsInstant(t *testing.T) {
	t.Parallel()
	desk, tool := refreshRig(t)
	read := refreshNow.Add(-time.Minute)
	for kind, at := range map[integration.Kind]time.Time{
		integration.KindCloudflare: read, integration.KindDatadog: refreshNow.Add(-collect.RefreshFloor),
	} {
		if err := desk.store.Save(t.Context(), collect.Status{Kind: kind, LastAttemptAt: at, Completed: true,
			LastCompleteAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	ada := refreshAs(boundTo("ada", iam.GrantAtlas))
	res, _ := callRefresh(t, tool, ada, "cloudflare")
	want := read.Add(collect.RefreshFloor).Format(time.RFC3339)
	if !res.Failed || !strings.Contains(res.Output, want) || desk.asks != 0 {
		t.Errorf("an ask inside the floor answered %q after %d writes, want a refusal naming %s", res.Output,
			desk.asks, want)
	}
	if res, out := callRefresh(t, tool, ada, "datadog"); res.Failed || out["changed"] != true {
		t.Errorf("an ask at the floor answered %q", res.Output)
	}
}

// ONLY A SOURCE THE COMPANY CONNECTS IS ASKED FOR, BY A PERSON HOLDING THE
// ATLAS: any other name is refused naming the sources there are, and somebody
// without the app, or an agent, asks for nothing.
func TestARefreshNamesAConnectedSourceAndIsAHoldersAndAPersons(t *testing.T) {
	t.Parallel()
	desk, tool := refreshRig(t)
	res, _ := callRefresh(t, tool, refreshAs(boundTo("ada", iam.GrantAtlas)), "github")
	if !res.Failed || !strings.Contains(res.Output, `"cloudflare" and "datadog"`) {
		t.Errorf("a source the company does not connect answered %q", res.Output)
	}
	if res, _ := callRefresh(t, tool, refreshAs(boundTo("eve")), "cloudflare"); !res.Failed {
		t.Errorf("somebody without the atlas asked for a read: %q", res.Output)
	}
	agent := iam.Principal{ID: boundTo("x").ID, Kind: iam.KindSeat, Seat: "agent-cto", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAtlas}}
	if res, _ := callRefresh(t, tool, refreshAs(agent), "cloudflare"); !res.Failed {
		t.Errorf("an agent asked for a read: %q", res.Output)
	}
	if desk.asks != 0 {
		t.Errorf("%d asks were recorded by callers who may make none", desk.asks)
	}
}
