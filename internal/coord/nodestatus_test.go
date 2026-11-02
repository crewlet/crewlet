package coord_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ABSENT IS NOT ZERO. A node that publishes no status (its status hook did not
// answer within its share of that beat, so its heartbeat carried the placement
// half alone) is not a node with no work in flight, and a confident 0 would
// draw an idle row for a process that is simply not saying.
func TestANodeThatPublishesNoStatusIsNotReadAsIdle(t *testing.T) {
	t.Parallel()
	for name, meta := range map[string]map[string]any{
		"no meta at all":             nil,
		"only placement":             {"roles": []string{"seats"}, "labels": map[string]any{}},
		"a status that is not a map": {coord.StatusKey: "busy"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, ok := coord.StatusFromMeta(meta); ok {
				t.Error("a node that said nothing was read as having reported")
			}
		})
	}
}

func TestAPublishedStatusRoundTrips(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	pm := uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")
	want := coord.NodeStatus{
		InFlight: 3, Draining: true, Posture: "shed", StartedAt: started,
		AdminPosture: iam.AdminServe,
		MCP: []coord.MCPServerStatus{
			{Server: "github", Shared: true, Started: 1, Tools: 12},
			{Server: "jira", Started: 2, Failed: 1, Tools: 9,
				Error: "exec: jira-mcp: not found", ErrorSeat: pm},
		},
	}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: want.Meta()})
	if !ok {
		t.Fatal("a published status read as absent")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// A KEY THIS BUILD DOES NOT KNOW IS IGNORED. A successor sharing the fleet
// publishes what it adds beside what this build reads, in whatever shape it
// chooses, and a status that failed to decode over it would read as absent —
// "not saying" — for as long as the successor ran.
func TestAStatusKeyFromANewerBuildDecodesHarmlessly(t *testing.T) {
	t.Parallel()
	newer := map[string]any{
		"in_flight": 2, "draining": false, "posture": "serve",
		"features":           []any{"a_feature_from_a_newer_build"},
		"from_a_newer_build": map[string]any{"nested": []any{1.5, "x"}},
	}
	got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: newer})
	if !ok {
		t.Fatal("a newer peer's status read as absent")
	}
	if want := (coord.NodeStatus{InFlight: 2, Posture: "serve"}); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// THE HEARTBEAT IS RE-SENT EVERY BEAT, so a server's failure text is bounded
// on the wire: a child's stderr can be any length, and every byte of it would
// ride every renew of the node's presence.
func TestAnMCPFailureIsClippedOnTheWire(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("e", 4*coord.MaxMCPErrorBytes)
	// BOTH SENTENCES A ROW CARRIES, because both are re-sent on every beat
	// and neither is the engine's to keep short: a waiting instance's names
	// every variable it waits for, and a company decides how many a seat's
	// block references and how long their names are.
	for name, tc := range map[string]struct {
		row  coord.MCPServerStatus
		text func(coord.MCPServerStatus) string
	}{
		"a failed instance's error": {
			row:  coord.MCPServerStatus{Server: "jira", Failed: 1, Error: long},
			text: func(m coord.MCPServerStatus) string { return m.Error },
		},
		"a waiting instance's wait": {
			row:  coord.MCPServerStatus{Server: "jira", Waiting: 1, Wait: long},
			text: func(m coord.MCPServerStatus) string { return m.Wait },
		},
	} {
		status := coord.NodeStatus{MCP: []coord.MCPServerStatus{tc.row}}
		got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
		if len(got.MCP) != 1 {
			t.Fatalf("%s: decoded %d mcp rows, want 1", name, len(got.MCP))
		}
		text := tc.text(got.MCP[0])
		if n := len(text); n > coord.MaxMCPErrorBytes {
			t.Errorf("%s travelled as %d bytes, over the %d-byte bound", name, n, coord.MaxMCPErrorBytes)
		}
		if !strings.HasSuffix(text, "…") {
			t.Errorf("%s, clipped, carries no marker: %q", name, text)
		}
	}
}

// THE SAME MAP IS READ BOTH SIDES OF A STORE ROUND TRIP, where an int
// becomes a float64. A reader that knew only the local shape would report
// every peer's in-flight count as zero — which is the one number this
// exists to carry.
func TestAStatusSurvivesTheJSONRoundTripTheLeaseStoreDoes(t *testing.T) {
	t.Parallel()
	want := coord.NodeStatus{
		InFlight: 7, Posture: "serve",
		MCP: []coord.MCPServerStatus{{Server: "github", Shared: true, Started: 1, Tools: 4}},
	}
	raw, err := json.Marshal(map[string]any{coord.StatusKey: want.Meta()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := coord.StatusFromMeta(meta)
	if !ok {
		t.Fatal("a round-tripped status read as absent")
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// AN UNSET POSTURE IS OMITTED rather than published as "": a node before a
// control plane exists has no posture, and an empty string in the payload
// would make the reading side unable to tell that from one it did not
// understand.
func TestAnUnsetPostureIsNotPublished(t *testing.T) {
	t.Parallel()
	meta := coord.NodeStatus{InFlight: 1}.Meta()
	if _, present := meta["posture"]; present {
		t.Errorf("an unset posture was published: %+v", meta)
	}
	if _, present := meta["started_at"]; present {
		t.Errorf("an unset start time was published: %+v", meta)
	}
	if _, present := meta["mcp"]; present {
		t.Errorf("an empty mcp was published: %+v", meta)
	}
	// The two that are always meaningful stay, including their zeros: a
	// node reporting zero turns in flight IS saying something.
	if _, present := meta["in_flight"]; !present {
		t.Error("in_flight was omitted")
	}
	if _, present := meta["draining"]; !present {
		t.Error("draining was omitted, so a node that is not draining says nothing")
	}
}

// THE ADMIN POSTURE IS ABSENT RATHER THAN EMPTY on a node that declares none.
//
// It is the same rule as the posture above and it matters more here, because
// what an empty cell would mean is the opposite of the truth: a worker node
// binds no API and therefore has no admin posture to state, while an empty
// string on the wire reads as a peer whose posture differs from every other
// node's — a disagreement drawn across a fleet that has none, on exactly the
// signal an operator is meant to trust. And each posture survives the JSON
// round trip the lease store does, under the key `/query/fleet` reads.
// Mutation: publish the field whatever its value and the first assertion
// goes red.
func TestANodeWithNoAdminPosturePublishesNone(t *testing.T) {
	t.Parallel()
	meta := coord.NodeStatus{InFlight: 1}.Meta()
	if _, present := meta["admin"]; present {
		t.Errorf("a node with no posture published one: %+v", meta)
	}
	for _, posture := range iam.AdminPostures {
		raw, err := json.Marshal(map[string]any{
			coord.StatusKey: coord.NodeStatus{AdminPosture: posture}.Meta(),
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"admin":"`+string(posture)+`"`) {
			t.Errorf("the wire carries %s, want the posture under \"admin\"", raw)
		}
		var round map[string]any
		if err := json.Unmarshal(raw, &round); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		got, ok := coord.StatusFromMeta(round)
		if !ok {
			t.Fatal("a published status read as absent")
		}
		if got.AdminPosture != posture {
			t.Errorf("admin posture = %q, want %q carried across the round trip",
				got.AdminPosture, posture)
		}
	}
}

// AN MCP FAILURE NAMES ITS SEAT BY ID, and a value that is not one names no
// seat. The row is read by every peer against its own chart, which keys a seat
// by its id, so a value no build of this one wrote — a handle, a mis-cased id
// — is decoded as "did not say" rather than as a seat a reader would then go
// looking for.
func TestAnMCPFailureNamesItsSeatByIDOrNotAtAll(t *testing.T) {
	t.Parallel()
	seat := uuid.MustParse("0b6f2c1e-9a4d-5c3b-8e7f-1a2b3c4d5e6f")
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "jira", Failed: 1, Error: "401", ErrorSeat: seat},
	}}
	meta := status.Meta()
	rows, _ := meta["mcp"].([]map[string]any)
	if len(rows) != 1 || rows[0]["error_seat"] != seat.String() {
		t.Fatalf("published %+v, want the seat's id on the row", meta["mcp"])
	}
	for name, wire := range map[string]any{
		"a handle":          "pm",
		"an upper-cased id": strings.ToUpper(seat.String()),
		"not a string":      42,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: map[string]any{
				"mcp": []any{map[string]any{"server": "jira", "failed": 1, "error_seat": wire}},
			}})
			if len(got.MCP) != 1 || got.MCP[0].ErrorSeat != uuid.Nil {
				t.Errorf("decoded %+v from %v, want the row with no seat named", got.MCP, wire)
			}
		})
	}
}

// modelBeat is a node's models and runners as a busy node reports them: one
// entry sitting out with parked seats, one refused and signed in on the host,
// and every runner cell in a different state.
func modelBeat() coord.NodeStatus {
	at := time.Date(2026, 10, 28, 21, 56, 0, 123456789, time.UTC)
	return coord.NodeStatus{
		Models: []coord.ModelStatus{{
			Key: "default", Plan: "3f9a1c0e22b1", SignIn: "token", Resolves: true,
			SittingOut: &coord.ModelSitOut{
				Until: at.Add(time.Hour), ResetsAt: at.Add(3 * time.Hour), Since: at,
				Window: "session", Said: "You've hit your session limit · resets 4:30pm (America/Vancouver)",
			},
			LastAnswer: at.Add(-time.Minute),
			Parked: []coord.ParkedSeat{
				{Seat: uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8"), Since: at.Add(time.Second)},
			},
			ParkedCount: 1, ParkedSince: at.Add(time.Second),
		}, {
			Key: "codex-review", SignIn: "credentials",
			LastFailure: &coord.ModelFailure{Kind: "auth", At: at, Said: "Not logged in. Run codex login."},
			HostLogin: &coord.HostLogin{State: coord.HostSignedIn, Method: "chatgpt",
				Account: "someone@example.com", CheckedAt: at},
		}},
		Runners: &coord.RunnerStatus{
			Direct:    coord.RunnerCell{State: coord.RunnerReady, Detail: "Landlock confinement; box root writable", CheckedAt: at},
			Container: coord.RunnerCell{State: coord.RunnerNotReady, Detail: "docker: not found", Fix: "Install Docker on node-2 and restart the engine", CheckedAt: at},
			E2B:       coord.RunnerCell{State: coord.RunnerUnconfigured, CheckedAt: at},
		},
	}
}

// WHAT A NODE KNOWS ABOUT ITS MODELS TRAVELS WHOLE, both on the local map and
// through the JSON round trip the lease store does: the Models page joins
// every node's rows, so a field lost on the wire is a sit-out, a refusal or a
// parked seat the page cannot show, and an instant cut to the second is two
// nodes' last answer and last failure ordered wrongly.
func TestAModelBeatRoundTrips(t *testing.T) {
	t.Parallel()
	want := modelBeat()
	local, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: want.Meta()})
	if !ok || !reflect.DeepEqual(local, want) {
		t.Errorf("local read = %+v (ok %v), want %+v", local, ok, want)
	}
	raw, err := json.Marshal(map[string]any{coord.StatusKey: want.Meta()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stored, ok := coord.StatusFromMeta(meta)
	if !ok || !reflect.DeepEqual(stored, want) {
		t.Errorf("after the store's round trip = %+v (ok %v), want %+v", stored, ok, want)
	}
}

// THE BEAT IS RE-SENT EVERY FIFTEEN SECONDS, so every piece of text a model row
// or a runner cell carries is bounded: a vendor's sentence, a harness's detail and
// an account are any length, and the plan is a prefix and never the whole
// sign-in identity.
func TestModelTextIsBoundedOnTheWire(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 4*coord.MaxModelSaidBytes)
	at := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	status := coord.NodeStatus{
		Models: []coord.ModelStatus{{
			Key: "default", Plan: strings.Repeat("ab", 32), SignIn: "token",
			SittingOut:  &coord.ModelSitOut{Until: at, Since: at, Window: "weekly", Said: long},
			LastFailure: &coord.ModelFailure{Kind: "auth", At: at, Said: long},
			HostLogin: &coord.HostLogin{State: coord.HostLoginUnknown, Method: long,
				Account: long, Detail: long, CheckedAt: at},
		}},
		Runners: &coord.RunnerStatus{Container: coord.RunnerCell{
			State: coord.RunnerNotReady, Detail: long, Fix: long, CheckedAt: at}},
	}
	got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
	if len(got.Models) != 1 || got.Runners == nil {
		t.Fatalf("decoded %+v, want one model row and the runners", got)
	}
	m := got.Models[0]
	if len(m.Plan) != coord.ModelPlanChars {
		t.Errorf("the plan travelled as %d characters, want the %d-character prefix", len(m.Plan), coord.ModelPlanChars)
	}
	for name, text := range map[string]string{
		"a sit-out's sentence": m.SittingOut.Said, "a failure's sentence": m.LastFailure.Said,
		"a login's method": m.HostLogin.Method, "a login's detail": m.HostLogin.Detail,
		"a runner's detail": got.Runners.Container.Detail, "a runner's fix": got.Runners.Container.Fix,
	} {
		if len(text) > coord.MaxModelSaidBytes || !strings.HasSuffix(text, "…") {
			t.Errorf("%s travelled as %d bytes (%q...), want at most %d and marked where it was cut",
				name, len(text), text[:min(len(text), 12)], coord.MaxModelSaidBytes)
		}
	}
	if n := len(m.HostLogin.Account); n > coord.MaxModelAccountBytes {
		t.Errorf("an account travelled as %d bytes, over the %d-byte bound", n, coord.MaxModelAccountBytes)
	}
}

// A ROW A READER CANNOT ATTRIBUTE IS DROPPED, never drawn: a model row naming
// no entry, and a parked seat naming no seat in canonical form. A node that
// sends no runners is not a node whose every cell is unconfigured.
func TestAModelRowNamingNothingIsDropped(t *testing.T) {
	t.Parallel()
	meta := map[string]any{coord.StatusKey: map[string]any{
		"in_flight": 0,
		"models": []any{
			map[string]any{"sign_in": "token"},
			map[string]any{"key": "default", "parked": []any{
				map[string]any{"seat": "not-a-seat", "since": "2026-10-28T00:00:00Z"},
				map[string]any{"seat": "6F1C2A3B-4D5E-5F60-8172-839405A6B7C8"},
			}},
		},
	}}
	got, ok := coord.StatusFromMeta(meta)
	if !ok {
		t.Fatal("the status read as absent")
	}
	if len(got.Models) != 1 || got.Models[0].Key != "default" {
		t.Fatalf("models = %+v, want the one row that names an entry", got.Models)
	}
	if len(got.Models[0].Parked) != 0 {
		t.Errorf("parked = %+v, want no seat from rows naming none canonically", got.Models[0].Parked)
	}
	if got.Runners != nil {
		t.Errorf("runners = %+v from a node that sent none, want nil", got.Runners)
	}
}

// A MODEL ROW'S PARKED SEATS ARE BOUNDED ON THE WIRE, and the count and the
// first wait travel whole: a hundred seats parked on one plan put eight ids on
// the beat, the longest-waiting, beside how many there are and when the first
// of them parked, which is what the waiting alarm is judged from.
//
// Mutation: publish every seat, or name the eight lowest ids rather than the
// longest-waiting, or drop the count, and this fails.
func TestAModelRowsParkedSeatsAreBounded(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	var parked []coord.ParkedSeat
	for i := range 100 {
		parked = append(parked, coord.ParkedSeat{
			Seat:  uuid.NewSHA1(uuid.NameSpaceURL, []byte{byte(i)}),
			Since: at.Add(-time.Duration(i) * time.Minute),
		})
	}
	status := coord.NodeStatus{Models: []coord.ModelStatus{{Key: "default", SignIn: "token", Parked: parked}}}
	got, _ := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
	row := got.Models[0]
	if len(row.Parked) != coord.MaxModelParkedSeats || row.ParkedCount != 100 {
		t.Fatalf("the row named %d seat(s) of %d, want %d of 100", len(row.Parked), row.ParkedCount,
			coord.MaxModelParkedSeats)
	}
	if want := at.Add(-99 * time.Minute); !row.ParkedSince.Equal(want) || !row.Parked[0].Since.Equal(want) {
		t.Errorf("the wait reads from %v (first named %v), want the longest-waiting seat's %v",
			row.ParkedSince, row.Parked[0].Since, want)
	}
}

// AN MCP ROW'S OUTAGE, CLASS AND RESTARTS CROSS THE LEASE whole, through the
// JSON round trip the lease store makes, and a healthy row carries none of
// them: absent reads as no outage, no class and no restart, which is every
// row of a company whose servers are up.
func TestAnMCPRowCarriesItsOutageClassAndRestartsAcrossTheLease(t *testing.T) {
	t.Parallel()
	down := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "tracker", Failed: 1, Error: "would not start", ErrorClass: "dns", DownSince: down, Restarts: 3},
		{Server: "search", Shared: true, Started: 1, Tools: 2},
	}}
	raw, err := json.Marshal(map[string]any{coord.StatusKey: status.Meta()})
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	got, ok := coord.StatusFromMeta(meta)
	if !ok || len(got.MCP) != 2 {
		t.Fatalf("read back %+v (%v)", got, ok)
	}
	if row := got.MCP[0]; row.ErrorClass != "dns" || !row.DownSince.Equal(down) || row.Restarts != 3 {
		t.Errorf("the failing row read back as %+v", row)
	}
	if row := got.MCP[1]; row.ErrorClass != "" || !row.DownSince.IsZero() || row.Restarts != 0 {
		t.Errorf("the healthy row read back as %+v", row)
	}
	rows, _ := status.Meta()["mcp"].([]map[string]any)
	for _, key := range []string{"error_class", "down_since", "restarts"} {
		if _, sent := rows[1][key]; sent {
			t.Errorf("a healthy row sends %q", key)
		}
	}
}
