package coord_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// recorded writes r through its rows and reads it back under its node, as a
// store that keeps bytes does: an int comes back a float64, a typed slice an
// []any and an instant a string, so a field the decoder knows in only one
// shape is lost here as it would be on a real store.
func recorded(t *testing.T, r coord.NodeStatusRecord) coord.NodeStatusRecord {
	t.Helper()
	rows, err := r.Rows()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := coord.AssembleNodeStatusRecords(rows, nil)
	if len(got) != 1 {
		t.Fatalf("the rows of %s assembled as %+v, want its record", r.Node, got)
	}
	return got[0]
}

// wireOf is r as a peer's decoder first meets it: its own row's JSON object,
// with every other row's object beside it as a part of its own, "mcp" and
// "models" each a list of their rows (their name under "server" and "key", as
// their key files it), and "growth".
func wireOf(t *testing.T, r coord.NodeStatusRecord) map[string]any {
	t.Helper()
	rows, err := r.Rows()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out map[string]any
	var mcp, models []any
	for _, row := range rows {
		var obj map[string]any
		if err := json.Unmarshal(row.Value, &obj); err != nil {
			t.Fatalf("the row %s is not a JSON object: %v", row.Key.Key(), err)
		}
		switch row.Key.Class {
		case coord.NodeStatusOwn:
			out = obj
		case coord.NodeStatusMCP:
			obj["server"] = row.Key.Name
			mcp = append(mcp, obj)
		case coord.NodeStatusModel:
			obj["key"] = row.Key.Name
			models = append(models, obj)
		case coord.NodeStatusGrowth:
			delete(obj, "epoch")
			out["growth"] = obj
		}
	}
	if mcp != nil {
		out["mcp"] = mcp
	}
	if models != nil {
		out["models"] = models
	}
	return out
}

// decoded reads rows written by hand, as a peer of another build might write
// them, filed under n1: wire is [wireOf]'s shape, each MCP and model row
// filed under its "server" or "key" (a row naming none is filed nowhere, as
// no key can name it), and every row at wire's epoch.
func decoded(t *testing.T, wire map[string]any) coord.NodeStatusRecord {
	t.Helper()
	own := map[string]any{}
	var rows []coord.NodeStatusRow
	row := func(class coord.NodeStatusClass, name string, body map[string]any) {
		if _, ok := body["epoch"]; !ok {
			body["epoch"] = wire["epoch"]
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		rows = append(rows, coord.NodeStatusRow{Key: coord.NodeStatusRowKey{Class: class, Node: "n1", Name: name},
			Value: raw})
	}
	for key, value := range wire {
		switch key {
		case "mcp", "models":
			class, name := coord.NodeStatusMCP, "server"
			if key == "models" {
				class, name = coord.NodeStatusModel, "key"
			}
			for _, item := range value.([]any) {
				body := item.(map[string]any)
				if named, _ := body[name].(string); named != "" {
					delete(body, name)
					row(class, named, body)
				}
			}
		case "growth":
			if body, ok := value.(map[string]any); ok {
				row(coord.NodeStatusGrowth, "", body)
			}
		default:
			own[key] = value
		}
	}
	row(coord.NodeStatusOwn, "", own)
	got := coord.AssembleNodeStatusRecords(rows, nil)
	if len(got) != 1 {
		t.Fatalf("the rows assembled as %+v, want n1's record", got)
	}
	return got[0]
}

// A NODE'S STATUS RECORD TRAVELS WHOLE: its epoch, its instant to the
// nanosecond, every MCP and model row, the runner cells and the infra summary,
// through the bytes a store keeps it as. The node is the key it is filed
// under, never a field of the body, so a record cannot describe another node.
func TestANodesStatusRecordRoundTripsWhole(t *testing.T) {
	t.Parallel()
	want := modelRecord()
	want.MCP = []coord.MCPServerStatus{
		{Server: "github", Shared: true, Started: 1, Tools: 12},
		{Server: "jira", Started: 2, Failed: 1, Tools: 9, Error: "exec: jira-mcp: not found",
			ErrorSeat: uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8"),
			Unlisted:  []string{"jira_add_coment"}},
	}
	want.Infra = coord.NodeInfra{Address: "10.0.0.4", Cluster: "acme", ObjectsBackend: "nats"}
	if got := recorded(t, want); !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	rows, err := want.Rows()
	if err != nil {
		t.Fatal(err)
	}
	for i, row := range rows {
		// THE NAMES ARE THE KEY'S: a body naming its node, server or entry
		// could describe another.
		for _, key := range []string{`"node"`, `"Node"`, `"server"`, `"key"`} {
			if strings.Contains(string(row.Value), key+":") {
				t.Errorf("the row %s carries %s: a name is the key a store files it under", row.Key.Key(), key)
			}
		}
		rows[i].Key.Node = "node-9"
	}
	if got := coord.AssembleNodeStatusRecords(rows, nil); len(got) != 1 || got[0].Node != "node-9" ||
		len(got[0].MCP) != 2 || len(got[0].Models) != len(want.Models) {
		t.Errorf("rows filed under node-9 read back as %+v", got)
	}
}

// A NODE SAYS WHICH OF ITS SEATS' RUNS WAIT FOR AN OPERATOR, so a node with no
// alarm table of its own (a satellite) is raised by those that have one: the
// count and the run waiting longest, its seat, its condition and since when,
// read back as written through the bytes a store keeps the record as; the seat
// and the kind bounded on the wire, since the record is rewritten for as long
// as the wait lasts. Nothing waiting is nothing on the wire, and a row that
// counts no run, or is no row, names none.
//
// Mutation: leave the row off the wire, read it back without its instant, or
// carry a seat past its bound, and a case goes red.
func TestAStatusRecordCarriesTheResumesWaitingForAnOperator(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 3, 4, 5, 6, 7, 891, time.UTC)
	want := coord.NodeStatusRecord{Node: "n1", Epoch: 2, At: since, ResumesBlocked: &coord.ResumesBlocked{
		Runs: 2, Seat: "swe", Kind: "disk_full", Since: since}}
	if got := recorded(t, want); !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, want %+v", got.ResumesBlocked, want.ResumesBlocked)
	}

	long := coord.NodeStatusRecord{Node: "n1", Epoch: 2, ResumesBlocked: &coord.ResumesBlocked{Runs: 1,
		Seat: strings.Repeat("s", 4*iam.MaxLogin), Kind: strings.Repeat("k", 4*coord.MaxResumeBlockedKindBytes),
		Since: since}}
	if b := recorded(t, long).ResumesBlocked; b == nil || len(b.Seat) > iam.MaxLogin ||
		len(b.Kind) > coord.MaxResumeBlockedKindBytes {
		t.Errorf("a seat of %d bytes and a kind of %d crossed as %+v, want them bounded to %d and %d",
			len(long.ResumesBlocked.Seat), len(long.ResumesBlocked.Kind), b, iam.MaxLogin,
			coord.MaxResumeBlockedKindBytes)
	}

	for name, r := range map[string]coord.NodeStatusRecord{
		"nothing waiting": {Node: "n1", Epoch: 2},
		"a row counting no run": {Node: "n1", Epoch: 2, ResumesBlocked: &coord.ResumesBlocked{Seat: "swe",
			Kind: "disk_full", Since: since}},
	} {
		if _, sent := wireOf(t, r)["resumes_blocked"]; sent {
			t.Errorf("%s: wrote a row of runs waiting for an operator", name)
		}
	}
	for name, row := range map[string]any{
		"a row that is no map":  "swe",
		"a row counting no run": map[string]any{"runs": 0, "seat": "swe", "kind": "disk_full"},
		"a row counting less":   map[string]any{"runs": -1, "seat": "swe", "kind": "disk_full"},
	} {
		if got := decoded(t, map[string]any{"epoch": 2, "resumes_blocked": row}); got.ResumesBlocked != nil {
			t.Errorf("%s: read as %+v, want no run named", name, got.ResumesBlocked)
		}
	}
}

// A NODE THAT RUNS NOTHING WRITES ITS EPOCH AND ITS INSTANT, and nothing that a
// reader could take for a row: no MCP, model, runner or infra key.
func TestARecordThatSaysNothingCarriesOnlyItsEpochAndInstant(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	wire := wireOf(t, coord.NodeStatusRecord{Node: "n1", Epoch: 4, At: at})
	if len(wire) != 2 || wire["epoch"] != float64(4) || wire["at"] == nil {
		t.Errorf("a record that says nothing is written as %v, want its epoch and its instant alone", wire)
	}
}

// A KEY THIS BUILD DOES NOT KNOW IS IGNORED, and a body that is not an object
// is refused: a successor writes what it adds beside what this build reads,
// and a record that failed to decode over it would draw the successor's node as
// not saying for as long as it ran.
func TestAStatusRecordFromANewerBuildDecodesHarmlessly(t *testing.T) {
	t.Parallel()
	got := decoded(t, map[string]any{
		"epoch": 3, "from_a_newer_build": map[string]any{"nested": []any{1.5, "x"}},
		"models": []any{map[string]any{"key": "default", "sign_in": "token", "feature": true}},
	})
	want := coord.NodeStatusRecord{Node: "n1", Epoch: 3,
		Models: []coord.ModelStatus{{Key: "default", SignIn: "token"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, want %+v", got, want)
	}
	for _, body := range []string{`[]`, `"x"`, `null`, `{`} {
		rows := []coord.NodeStatusRow{{Key: coord.NodeStatusRowKey{Class: coord.NodeStatusOwn, Node: "n1"},
			Value: []byte(body)}}
		if got := coord.AssembleNodeStatusRecords(rows, nil); len(got) != 0 {
			t.Errorf("the body %s decoded as a record: %+v", body, got)
		}
	}
}

// A RECORD NAMES ITS NODE AND THE PRESENCE EPOCH IT WAS WRITTEN UNDER, or no
// store files it: a record at epoch zero is one no reader could ever count.
func TestARecordWithoutItsNodeOrEpochIsRefused(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]coord.NodeStatusRecord{
		"no node":  {Epoch: 1},
		"no epoch": {Node: "n1"},
	} {
		if err := r.Check(); !errors.Is(err, coord.ErrNoNodeStatus) {
			t.Errorf("%s: Check = %v, want ErrNoNodeStatus", name, err)
		}
	}
	if err := (coord.NodeStatusRecord{Node: "n1", Epoch: 1}).Check(); err != nil {
		t.Errorf("a record naming its node and epoch was refused: %v", err)
	}
}

// A RECORD COUNTS ONLY WHILE ITS NODE HOLDS PRESENCE AT THE RECORD'S EPOCH:
// a node whose presence went, and a node that came back at a new epoch, are
// read as not saying until the live incarnation writes, never as the rows an
// earlier incarnation left. The contract suite runs the same rule over every
// backend's leases (coordtest's node status cases).
//
// Mutation: compare the node alone, and the restarted node's old record
// counts.
func TestLiveNodeStatusesCountsARecordOnlyAtItsNodesPresenceEpoch(t *testing.T) {
	t.Parallel()
	presence := []coord.Lease{
		{Resource: coord.NodeResource("live"), Epoch: 5},
		{Resource: coord.NodeResource("restarted"), Epoch: 9},
		{Resource: coord.NodeResource("silent"), Epoch: 2},
		{Resource: coord.SeatResource(uuid.MustParse("0a1b2c3d-4e5f-5a6b-8c7d-8e9fa0b1c2d3")), Epoch: 5},
	}
	records := []coord.NodeStatusRecord{
		{Node: "live", Epoch: 5}, {Node: "restarted", Epoch: 8}, {Node: "gone", Epoch: 1},
		{Node: "0a1b2c3d-4e5f-5a6b-8c7d-8e9fa0b1c2d3", Epoch: 5},
	}
	got := coord.LiveNodeStatuses(records, presence)
	if len(got) != 1 || got["live"].Epoch != 5 {
		t.Errorf("counted %v, want the live node's record alone", got)
	}
}

// A SERVER'S FAILURE TEXT IS BOUNDED ON THE WIRE: a child's stderr can be any
// length, and every byte of it would ride every rewrite of the node's status
// record, at least one every [coord.ReconcileInterval].
func TestAnMCPFailureIsClippedOnTheWire(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("e", 4*coord.MaxMCPErrorBytes)
	// BOTH SENTENCES A ROW CARRIES, because both are rewritten with the
	// record and neither is the engine's to keep short: a waiting instance's names
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
		got := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{tc.row}})
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

// AN MCP FAILURE NAMES ITS SEAT BY ID, and a value that is not one names no
// seat. The row is read by every peer against its own chart, which keys a seat
// by its id, so a value no build of this one wrote — a handle, a mis-cased id
// — is decoded as "did not say" rather than as a seat a reader would then go
// looking for.
func TestAnMCPFailureNamesItsSeatByIDOrNotAtAll(t *testing.T) {
	t.Parallel()
	seat := uuid.MustParse("0b6f2c1e-9a4d-5c3b-8e7f-1a2b3c4d5e6f")
	meta := wireOf(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{
		{Server: "jira", Failed: 1, Error: "401", ErrorSeat: seat},
	}})
	rows, _ := meta["mcp"].([]any)
	if row, _ := rows[0].(map[string]any); len(rows) != 1 || row["error_seat"] != seat.String() {
		t.Fatalf("published %+v, want the seat's id on the row", meta["mcp"])
	}
	for name, wire := range map[string]any{
		"a handle":          "pm",
		"an upper-cased id": strings.ToUpper(seat.String()),
		"not a string":      42,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := decoded(t, map[string]any{"epoch": 1,
				"mcp": []any{map[string]any{"server": "jira", "failed": 1, "error_seat": wire}},
			})
			if len(got.MCP) != 1 || got.MCP[0].ErrorSeat != uuid.Nil {
				t.Errorf("decoded %+v from %v, want the row with no seat named", got.MCP, wire)
			}
		})
	}
}

// modelRecord is a node's models and runners as a busy node reports them: one
// entry sitting out with parked seats, one refused and signed in on the host,
// and every runner cell in a different state.
func modelRecord() coord.NodeStatusRecord {
	at := time.Date(2026, 10, 28, 21, 56, 0, 123456789, time.UTC)
	return coord.NodeStatusRecord{
		Node: "node-2", Epoch: 7, At: at,
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
			Limits: []coord.ModelLimit{
				{Window: "session", Hours: 5, State: "reached", Used: ptr(1.02), ResetsAt: at.Add(3 * time.Hour), Seen: at},
				{Window: "weekly", Hours: 168, State: "ok", Used: ptr(0.0), Seen: at, Read: true},
				{Window: "model", Hours: 168, Model: "opus", State: "warning", Seen: at.Add(-time.Minute)},
			},
		}, {
			Key: "review", SignIn: "credentials",
			LastFailure: &coord.ModelFailure{Kind: "auth", At: at, Said: "Not logged in. Run codex login."},
			HostLogin: &coord.HostLogin{State: coord.HostSignedIn, Method: "chatgpt",
				Account: "someone@example.com", CheckedAt: at},
		}},
		Runners: &coord.RunnerStatus{
			Direct:    coord.RunnerCell{State: coord.RunnerReady, Detail: "Landlock confinement; box root writable", CheckedAt: at},
			Container: coord.RunnerCell{State: coord.RunnerNotReady, Detail: "docker: not found", Fix: "Install Docker on node-2 and restart the engine", CheckedAt: at},
		},
	}
}

// WHAT A NODE KNOWS ABOUT ITS MODELS TRAVELS WHOLE, through the bytes a store
// keeps its status record as: the Models page joins
// every node's rows, so a field lost on the wire is a sit-out, a refusal or a
// parked seat the page cannot show, and an instant cut to the second is two
// nodes' last answer and last failure ordered wrongly.
func TestAModelRecordRoundTrips(t *testing.T) {
	t.Parallel()
	want := modelRecord()
	if stored := recorded(t, want); !reflect.DeepEqual(stored, want) {
		t.Errorf("after the store's round trip = %+v, want %+v", stored, want)
	}
}

// THE RECORD IS REWRITTEN AT LEAST EVERY FIFTEEN SECONDS, so every piece of text a model row
// or a runner cell carries is bounded: a vendor's sentence, a harness's detail and
// an account are any length, and the plan is a prefix and never the whole
// sign-in identity.
func TestModelTextIsBoundedOnTheWire(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 4*coord.MaxModelSaidBytes)
	at := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	status := coord.NodeStatusRecord{
		Node: "n1", Epoch: 1,
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
	got := recorded(t, status)
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
	got := decoded(t, map[string]any{
		"epoch": 1,
		"models": []any{
			map[string]any{"sign_in": "token"},
			map[string]any{"key": "default", "parked": []any{
				map[string]any{"seat": "not-a-seat", "since": "2026-10-28T00:00:00Z"},
				map[string]any{"seat": "6F1C2A3B-4D5E-5F60-8172-839405A6B7C8"},
			}},
		},
	})
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
// the record, the longest-waiting, beside how many there are and when the first
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
	got := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1,
		Models: []coord.ModelStatus{{Key: "default", SignIn: "token", Parked: parked}}})
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

// A MODEL ROW'S FLEET HOMES TRAVEL the most recently taken first, every one
// its row holds: ninety-six short ones whole and uncounted; a peer's home
// naming no fleet or subject is dropped, as it steers no pick. Mutation:
// publish the oldest first, or cap the list below what the row holds, and
// this fails.
func TestAModelRowsFleetHomesAreTheNewestFirst(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	var homes []coord.FleetHome
	for i := range 96 {
		homes = append(homes, coord.FleetHome{Fleet: "opus", Subject: fmt.Sprintf("seat-%d", i),
			Job: "execute", At: at.Add(-time.Duration(i) * time.Minute)})
	}
	slices.Reverse(homes)
	got := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1,
		Models: []coord.ModelStatus{{Key: "max", SignIn: "token", Homes: homes}}})
	row := got.Models[0]
	if len(row.Homes) != len(homes) || row.HomesCount != 0 {
		t.Fatalf("the row carried %d home(s) counted %d, want all %d uncounted", len(row.Homes), row.HomesCount,
			len(homes))
	}
	if h := row.Homes[0]; h != homes[len(homes)-1] {
		t.Errorf("first home = %+v, want the newest whole: %+v", h, homes[len(homes)-1])
	}
	wire := map[string]any{"node": "n1", "epoch": 1, "at": at.Format(time.RFC3339Nano),
		"models": []any{map[string]any{"key": "max", "homes": []any{
			map[string]any{"subject": "engineer"}, map[string]any{"fleet": "opus"}, "nonsense",
			map[string]any{"fleet": "opus", "subject": "engineer", "job": "execute", "at": at.Format(time.RFC3339Nano)},
		}}}}
	if read := decoded(t, wire).Models[0].Homes; len(read) != 1 || read[0].Subject != "engineer" ||
		!read[0].At.Equal(at) {
		t.Errorf("a peer's homes read %+v, want the one naming its fleet and subject", read)
	}
}

// A MODEL ROW'S LIMIT WINDOWS ARE BOUNDED on the wire, and one naming no window
// or state is dropped, since no reader can draw it.
func TestAModelRowsLimitWindowsAreBounded(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC)
	var limits []coord.ModelLimit
	for i := range 3 * coord.MaxModelLimits {
		limits = append(limits, coord.ModelLimit{Window: "model", Model: strings.Repeat("x", i+40), State: "ok", Seen: at})
	}
	got := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1,
		Models: []coord.ModelStatus{{Key: "default", SignIn: "token", Limits: limits}}})
	row := got.Models[0]
	if len(row.Limits) != coord.MaxModelLimits {
		t.Fatalf("the row carried %d window(s), want %d", len(row.Limits), coord.MaxModelLimits)
	}
	if len(row.Limits[0].Model) > coord.MaxModelLimitModelBytes {
		t.Errorf("a window's model is %d bytes on the wire, want at most %d", len(row.Limits[0].Model),
			coord.MaxModelLimitModelBytes)
	}
	wire := map[string]any{"node": "n1", "epoch": 1, "at": at.Format(time.RFC3339Nano),
		"models": []any{map[string]any{"key": "default", "limits": []any{
			map[string]any{"state": "ok"}, map[string]any{"window": "weekly"}, "nonsense",
			map[string]any{"window": "weekly", "state": "ok", "used": 0.4},
		}}}}
	if read := decoded(t, wire).Models[0].Limits; len(read) != 1 || *read[0].Used != 0.4 {
		t.Errorf("a peer's limits read %+v, want the one window naming its kind and state", read)
	}
}

func ptr[T any](v T) *T { return &v }

// AN MCP ROW'S OUTAGE, WAIT, CLASS AND RESTARTS CROSS THE STORE whole, through
// the bytes a store keeps a status record as, and a healthy row carries none of
// them: absent reads as no outage, no wait, no class and no restart, which is
// every row of a company whose servers are up.
func TestAnMCPRowCarriesItsOutageClassAndRestartsAcrossTheStore(t *testing.T) {
	t.Parallel()
	down := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	held := down.Add(-time.Hour)
	status := coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{
		{Server: "tracker", Failed: 1, Error: "would not start", ErrorClass: "dns", DownSince: down, Restarts: 3,
			Waiting: 1, Wait: "waits for ${GITHUB_TOKEN}", WaitingSince: held},
		{Server: "web-search", Shared: true, Started: 1, Tools: 2},
	}}
	got := recorded(t, status)
	if len(got.MCP) != 2 {
		t.Fatalf("read back %+v", got)
	}
	if row := got.MCP[0]; row.ErrorClass != "dns" || !row.DownSince.Equal(down) || row.Restarts != 3 ||
		!row.WaitingSince.Equal(held) {
		t.Errorf("the failing row read back as %+v", row)
	}
	if row := got.MCP[1]; row.ErrorClass != "" || !row.DownSince.IsZero() || row.Restarts != 0 ||
		!row.WaitingSince.IsZero() {
		t.Errorf("the healthy row read back as %+v", row)
	}
	rows, _ := wireOf(t, status)["mcp"].([]any)
	healthy, _ := rows[1].(map[string]any)
	for _, key := range []string{"error_class", "down_since", "restarts", "waiting_since"} {
		if _, sent := healthy[key]; sent {
			t.Errorf("a healthy row sends %q", key)
		}
	}
}

// A NODE SAYS WHAT ITS GROWTH PRODUCTS MET AT THEIR VENDORS, so every node's
// platform notices read the fleet's: each clock skew and deprecation read back
// as written through the bytes a store keeps the record as, each list and name
// bounded on the wire, nothing met nothing on the wire, and a row naming no
// product, or a deprecation no version, dropped.
//
// Mutation: leave the signals off the wire, or carry a list past its bound,
// and a case goes red.
func TestAStatusRecordCarriesItsGrowthSignals(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 20, 9, 0, 0, 7, time.UTC)
	want := coord.NodeStatusRecord{Node: "n1", Epoch: 2, At: since, Growth: &coord.GrowthSignals{
		ClockSkews: []coord.GrowthClockSkew{{Product: "appstoreconnect", Vendor: "Apple", OffsetSeconds: 95,
			Since: since}},
		Deprecations: []coord.GrowthDeprecation{{Product: "metaads", Version: "v23.0",
			Retires: time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), Since: since},
			{Product: "appleads", Version: "v5", Since: since}},
	}}
	if got := recorded(t, want); !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v, want %+v", got.Growth, want.Growth)
	}
	many := coord.NodeStatusRecord{Node: "n1", Epoch: 2, Growth: &coord.GrowthSignals{}}
	for range coord.MaxGrowthSignals + 5 {
		many.Growth.ClockSkews = append(many.Growth.ClockSkews, coord.GrowthClockSkew{
			Product: strings.Repeat("p", 4*coord.MaxGrowthSignalBytes), OffsetSeconds: -70, Since: since})
		many.Growth.Deprecations = append(many.Growth.Deprecations, coord.GrowthDeprecation{
			Product: "metaads", Version: strings.Repeat("v", 4*coord.MaxGrowthSignalBytes), Since: since})
	}
	g := recorded(t, many).Growth
	if g == nil || len(g.ClockSkews) != coord.MaxGrowthSignals || len(g.Deprecations) != coord.MaxGrowthSignals ||
		len(g.ClockSkews[0].Product) > coord.MaxGrowthSignalBytes ||
		len(g.Deprecations[0].Version) > coord.MaxGrowthSignalBytes {
		t.Errorf("%d signals of %d-byte names crossed unbounded", coord.MaxGrowthSignals+5,
			4*coord.MaxGrowthSignalBytes)
	}
	if _, sent := wireOf(t, coord.NodeStatusRecord{Node: "n1", Epoch: 2,
		Growth: &coord.GrowthSignals{}})["growth"]; sent {
		t.Error("a node that met nothing wrote growth signals")
	}
	got := decoded(t, map[string]any{"epoch": 2, "growth": map[string]any{
		"clock_skews":  []any{map[string]any{"vendor": "Apple", "offset_seconds": 90}, "x"},
		"deprecations": []any{map[string]any{"product": "metaads"}},
	}})
	if got.Growth != nil {
		t.Errorf("rows naming nothing read as %+v, want no signal", got.Growth)
	}
}

// A MODEL ROW'S WORK IN FLIGHT TRAVELS as far as its row holds, pinned work
// naming no fleet: a hundred short calls whole and uncounted; a peer's work
// naming no subject serves nobody and is dropped. Mutation: drop the running
// list from the row, or cap it below what the row holds, and this fails.
func TestAModelRowsWorkInFlightTravelsAsFarAsItsRowHolds(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	var running []coord.FleetRunning
	for i := range 100 {
		running = append(running, coord.FleetRunning{Fleet: "opus", Subject: fmt.Sprintf("seat-%d", i),
			Job: "review", Since: since.Add(time.Duration(i) * time.Second)})
	}
	running[1].Fleet = ""
	got := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1,
		Models: []coord.ModelStatus{{Key: "max", SignIn: "token", Running: running}}})
	row := got.Models[0]
	if len(row.Running) != len(running) || row.RunningCount != 0 {
		t.Fatalf("the row carried %d running counted %d, want all %d uncounted", len(row.Running),
			row.RunningCount, len(running))
	}
	if row.Running[0] != running[0] || row.Running[1] != running[1] {
		t.Errorf("running = %+v, want the first whole, pinned work with no fleet", row.Running[:2])
	}
	wire := map[string]any{"node": "n1", "epoch": 1, "at": since.Format(time.RFC3339Nano),
		"models": []any{map[string]any{"key": "max", "running": []any{
			map[string]any{"fleet": "opus"}, "nonsense",
			map[string]any{"subject": "engineer", "job": "review", "since": since.Format(time.RFC3339Nano)},
		}}}}
	if read := decoded(t, wire).Models[0].Running; len(read) != 1 || read[0].Subject != "engineer" ||
		read[0].Fleet != "" || !read[0].Since.Equal(since) {
		t.Errorf("decoded running = %+v, want engineer's pinned review alone", read)
	}
}
