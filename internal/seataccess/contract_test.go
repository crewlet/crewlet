package seataccess_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE DOCUMENT AND THE WIRE CANNOT SPELL A SETTING TWO WAYS: every lane's
// block reads back as it was written.
func TestDesiredRoundTripsThroughTheDocument(t *testing.T) {
	t.Parallel()
	cases := map[seataccess.Lane]seataccess.Desired{
		seataccess.LaneDatadog:    {Enabled: true, Level: "role-1"},
		seataccess.LaneGitLab:     {Enabled: true, Level: "30", Scope: []string{"acme/api", "acme/web"}},
		seataccess.LaneJira:       {Enabled: true, Level: "10101", Scope: []string{"OPS"}},
		seataccess.LaneConfluence: {Enabled: false, Level: "create:page read:space", Scope: []string{"ENG"}},
		seataccess.LaneMattermost: {Enabled: true, Level: "system_user system_post_all", Scope: []string{"town-square"}},
		seataccess.LaneSlack:      {Enabled: true},
		seataccess.LaneGitHub: {Enabled: true, Scope: []string{"acme/api"},
			Permissions: seataccess.Permissions{"contents": seataccess.AccessRead, "workflows": seataccess.AccessWrite}},
	}
	for lane, want := range cases {
		role := &config.Role{Name: "SRE", Handle: "sre"}
		if err := seataccess.SetDesired(role, lane, want); err != nil {
			t.Fatalf("%s: SetDesired: %v", lane, err)
		}
		got, present := seataccess.DesiredOf(role, lane)
		if !present || got.Enabled != want.Enabled || !got.SameAccess(want) {
			t.Errorf("%s: read back %+v (present %v), want %+v", lane, got, present, want)
		}
	}
	gl := &config.Role{Handle: "sre"}
	if err := seataccess.SetDesired(gl, seataccess.LaneGitLab, seataccess.Desired{Enabled: true, Level: "member_role:7"}); err != nil {
		t.Fatal(err)
	}
	if gl.Integrations.GitLab.MemberRoleID != 7 || gl.Integrations.GitLab.AccessLevel != 0 {
		t.Fatalf("custom role written as %+v", gl.Integrations.GitLab)
	}
	if err := seataccess.SetDesired(gl, seataccess.LaneGitLab, seataccess.Desired{Level: "35"}); err == nil {
		t.Fatal("a GitLab level GitLab does not have was written")
	}
}

// AN ABSENT BLOCK IS OFF (D9), and so is one with no `enabled`.
func TestAnAbsentBlockIsOff(t *testing.T) {
	t.Parallel()
	role := &config.Role{Handle: "sre", Integrations: config.RoleIntegrations{
		Datadog: &config.RoleDatadog{Role: "role-1"},
	}}
	if d, present := seataccess.DesiredOf(role, seataccess.LaneDatadog); !present || d.Enabled {
		t.Fatalf("a block with no enabled = %+v, want present and off", d)
	}
	if d, present := seataccess.DesiredOf(role, seataccess.LaneGitLab); present || d.Enabled {
		t.Fatalf("an absent block = %+v present %v, want off and absent", d, present)
	}
}

// ONE NAMING FUNCTION: the sink, the connect form and the tool binding agree.
func TestSinkNameIsTheConnectFormsName(t *testing.T) {
	t.Parallel()
	for _, handle := range []string{"sre", "senior-engineer", "nova.1", "7th"} {
		got := seataccess.SinkName(integration.KindDatadog, "APP_KEY", handle)
		want := setup.SecretNameFor(integration.KindDatadog, setup.Requirement{Field: "APP_KEY", Seat: handle})
		if got != want {
			t.Errorf("%s: SinkName %q, connect form %q", handle, got, want)
		}
	}
	if got := seataccess.SinkName(integration.KindAtlassian, "TOKEN", "sre"); got != "ATLASSIAN_TOKEN_SRE" {
		t.Errorf("SinkName = %q", got)
	}
}

// THE STEP-UP RULE (D10), derived from the vendor's own facts.
func TestNeedsStepUp(t *testing.T) {
	t.Parallel()
	opts := seataccess.Options{
		Levels: []seataccess.Level{{ID: "std"}, {ID: "admin", Administers: true}},
		Permissions: []seataccess.PermissionSpec{
			{Key: "contents", AdministersOnWrite: true}, {Key: "issues"},
		},
	}
	cases := []struct {
		name          string
		before, after seataccess.Desired
		want          bool
	}{
		{"a raise to an admin role", on("std"), on("admin"), true},
		{"turning on at an admin role", seataccess.Desired{}, on("admin"), true},
		{"keeping an admin role", on("admin"), on("admin"), false},
		{"a lowering", on("admin"), on("std"), false},
		{"turning off", on("admin"), seataccess.Desired{Level: "admin"}, false},
		{"write on contents", seataccess.Desired{Enabled: true}, seataccess.Desired{Enabled: true,
			Permissions: seataccess.Permissions{"contents": seataccess.AccessWrite}}, true},
		{"write on issues", seataccess.Desired{Enabled: true}, seataccess.Desired{Enabled: true,
			Permissions: seataccess.Permissions{"issues": seataccess.AccessWrite}}, false},
	}
	for _, tc := range cases {
		if got := seataccess.NeedsStepUp(opts, tc.before, tc.after); got != tc.want {
			t.Errorf("%s: NeedsStepUp = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE STEP-UP FAILS CLOSED (D10): with the vendor's role list unread, the
// offer vouches for no level, so any move to one needs the step-up, however
// harmless it may turn out to be; and a level the vendor does not list is
// treated the same. Keeping a level and turning a seat off still need none.
// What the tab marks as an administration role claims only what the offer
// shows.
//
// Mutation: judge an unlisted level as not administering, and a machine
// token raises a seat to the vendor's admin role while its list is unread.
func TestTheStepUpFailsClosedOnALevelTheOfferCannotVouchFor(t *testing.T) {
	t.Parallel()
	unread := seataccess.Options{Live: false}
	listed := seataccess.Options{Live: true, Levels: []seataccess.Level{{ID: "std"}}}
	for _, tc := range []struct {
		name          string
		opts          seataccess.Options
		before, after seataccess.Desired
		want          bool
	}{
		{"unread, a raise", unread, on("std"), on("admin"), true},
		{"unread, turning on", unread, seataccess.Desired{}, on("std"), true},
		{"unread, keeping the level", unread, on("admin"), on("admin"), false},
		{"unread, turning off", unread, on("admin"), seataccess.Desired{Level: "admin"}, false},
		{"unread, product access only", unread, seataccess.Desired{}, on(""), false},
		{"a level the vendor does not list", listed, on("std"), on("owner"), true},
	} {
		if got := seataccess.NeedsStepUp(tc.opts, tc.before, tc.after); got != tc.want {
			t.Errorf("%s: NeedsStepUp = %v, want %v", tc.name, got, tc.want)
		}
	}
	admin := seataccess.Options{Levels: []seataccess.Level{{ID: "std"}, {ID: "admin", Administers: true}}}
	if !seataccess.Administers(admin, on("admin")) || seataccess.Administers(admin, on("std")) {
		t.Error("Administers does not follow the vendor's own marking")
	}
	if seataccess.Administers(unread, on("admin")) {
		t.Error("an unread offer marked a seat as holding an administration role")
	}
	if seataccess.Administers(admin, seataccess.Desired{Level: "admin"}) {
		t.Error("a seat that is off was marked as administering")
	}
}

// THE TOOL STATE THE TAB REPORTS PER SEAT (D15).
func TestToolStateOf(t *testing.T) {
	t.Parallel()
	perSeat := &config.Company{MCPServers: []config.MCPServer{
		{Name: "datadog", Transport: config.TransportHTTP, URL: "https://mcp.datadoghq.com/v1/mcp", Shared: org.Off()},
	}}
	none := &config.Company{}
	// TURNED OFF, the server reads nobody's credential: the lane is not
	// live however well the credential is sealed, and says why.
	turnedOff := &config.Company{MCPServers: []config.MCPServer{
		{Name: "datadog", Transport: config.TransportHTTP, URL: "https://mcp.datadoghq.com/v1/mcp",
			Shared: org.Off(), Enabled: org.Off()},
	}}
	live := held("a")
	waiting := held("a")
	waiting.Credential = nil
	cases := []struct {
		name string
		c    *config.Company
		lane seataccess.Lane
		want seataccess.Desired
		have seataccess.Observed
		tool seataccess.ToolState
	}{
		{"off", perSeat, dd, seataccess.Desired{}, live, seataccess.ToolOff},
		{"builtin", perSeat, seataccess.LaneJira, on("a"), live, seataccess.ToolBuiltin},
		{"no server declared", none, dd, on("a"), live, seataccess.ToolNoServer},
		{"waiting for its key", perSeat, dd, on("a"), waiting, seataccess.ToolWaiting},
		{"live", perSeat, dd, on("a"), live, seataccess.ToolLive},
		{"its only server turned off", turnedOff, dd, on("a"), live, seataccess.ToolServerOff},
		{"its only server turned off, still off", turnedOff, dd, seataccess.Desired{}, live, seataccess.ToolOff},
	}
	for _, tc := range cases {
		if got := seataccess.ToolStateOf(tc.c, tc.lane, tc.want, tc.have); got != tc.tool {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.tool)
		}
	}
}

// THE LEDGER ROUND-TRIPS THROUGH THE COORDINATION STORE, per integration.
func TestCoordLedgerRoundTrips(t *testing.T) {
	t.Parallel()
	ledger, err := seataccess.NewCoordLedger(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	rec := seataccess.Record{
		Kind: integration.KindDatadog, Handle: "sre", Name: "SRE",
		Account: &seataccess.Account{ID: "u-1", State: seataccess.AccountActive, CreatedAt: at},
		Lanes:   map[seataccess.Lane]seataccess.LaneRecord{dd: {Observed: held("a")}},
	}
	if err := ledger.Put(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Records(t.Context(), integration.KindDatadog)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(rec)
	b, _ := json.Marshal(got["sre"])
	if string(a) != string(b) {
		t.Fatalf("read back %s, want %s", b, a)
	}
	if other, _ := ledger.Records(t.Context(), integration.KindGitLab); len(other) != 0 {
		t.Fatalf("gitlab listed %v", other)
	}
	if err := ledger.Drop(t.Context(), integration.KindDatadog, "sre"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ledger.Records(t.Context(), integration.KindDatadog); len(got) != 0 {
		t.Fatalf("after drop: %v", got)
	}
	if _, err := seataccess.NewCoordLedger(nil); err == nil {
		t.Fatal("a ledger was built on no store")
	}
}

// THE REGISTRY REFUSES A VENDOR WHOSE CATALOG DOES NOT MATCH ITS KIND.
func TestRegisterRefusesAMismatchedCatalog(t *testing.T) {
	t.Parallel()
	mustPanic := func(name string, v seataccess.Vendor) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Errorf("%s: Register did not panic", name)
			}
		}()
		seataccess.Register(v)
	}
	mustPanic("a kind with no per-seat access", seataccesstest.NewVendor(integration.KindJira))
	mustPanic("no credential field", &seataccesstest.Vendor{KindValue: integration.KindDatadog})
}

// KINDS AND LANES: Atlassian is the one kind with two lanes.
func TestLanesOfEveryKind(t *testing.T) {
	t.Parallel()
	if got := seataccess.LanesOf(integration.KindAtlassian); !slices.Equal(got,
		[]seataccess.Lane{seataccess.LaneJira, seataccess.LaneConfluence}) {
		t.Fatalf("atlassian lanes = %v", got)
	}
	for _, kind := range seataccess.Kinds() {
		if !kind.Valid() {
			t.Errorf("%q is not an integration kind", kind)
		}
		if len(seataccess.LanesOf(kind)) == 0 {
			t.Errorf("%s has no lanes", kind)
		}
	}
}

// TWO NODES READ ONE LEDGER: what the node running the sync records, the node
// serving the access route reads, because both are on the fleet's store.
func TestTwoNodesShareTheLedger(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	syncing, err := seataccess.NewCoordLedger(fleet)
	if err != nil {
		t.Fatal(err)
	}
	serving, err := seataccess.NewCoordLedger(fleet)
	if err != nil {
		t.Fatal(err)
	}
	rec := seataccess.Record{Kind: integration.KindGitLab, Handle: "sre",
		Account: &seataccess.Account{ID: "u-7", State: seataccess.AccountActive, CreatedAt: at}}
	if err := syncing.Put(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := serving.Records(t.Context(), integration.KindGitLab)
	if err != nil {
		t.Fatal(err)
	}
	if got["sre"].Account == nil || got["sre"].Account.ID != "u-7" {
		t.Fatalf("the serving node read %+v, want the row the syncing node wrote", got["sre"])
	}
}
