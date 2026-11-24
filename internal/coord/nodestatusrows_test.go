package coord_test

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/textcut"
)

// sixFold is n bytes JSON writes as six each.
func sixFold(n int) string { return strings.Repeat("\x01", n) }

// widestRecord is a node's status with every field at the most its writer
// keeps: every text far past its bound in plain bytes, which a cut keeps at
// exactly its bound as stored (an escaped one stores at most that), every
// list past its bound, every count and epoch at its largest, every instant to
// the nanosecond and a float at its longest.
func widestRecord(unlisted int) coord.NodeStatusRecord {
	long := strings.Repeat("a", 4096)
	seat := uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")
	cell := coord.RunnerCell{State: coord.RunnerState(long), Detail: long, Fix: long, CheckedAt: widestInstant}
	servers := make([]string, 4*coord.MaxInfraServers)
	for i := range servers {
		servers[i] = long
	}
	mcp := coord.MCPServerStatus{Server: "jira", Shared: true, Started: math.MaxInt, Failed: math.MaxInt,
		Waiting: math.MaxInt, Tools: math.MaxInt, Error: long, ErrorSeat: seat, ErrorClass: long,
		DownSince: widestInstant, Restarts: math.MaxInt, Wait: long, WaitSeat: seat, WaitingSince: widestInstant,
		RefusedCount: math.MaxInt, ShadowedCount: math.MaxInt}
	for i := range 4 * coord.MaxMCPToolNotes {
		mcp.Refused = append(mcp.Refused, coord.MCPToolRefusal{Tool: long, Reason: long + fmt.Sprint(i)})
		mcp.Shadowed = append(mcp.Shadowed, coord.MCPToolShadow{Tool: long, By: long + fmt.Sprint(i)})
	}
	for i := range unlisted {
		mcp.Unlisted = append(mcp.Unlisted, fmt.Sprintf("%s%06d", sixFold(coord.MaxMCPToolNameBytes-6), i))
	}
	used := -1.2345678901234567e-06
	model := coord.ModelStatus{Key: "default", Plan: long, SignIn: long, Resolves: true,
		SittingOut: &coord.ModelSitOut{Until: widestInstant, ResetsAt: widestInstant, Since: widestInstant,
			Window: long, Said: long},
		LastAnswer:  widestInstant,
		LastFailure: &coord.ModelFailure{Kind: long, At: widestInstant, Said: long},
		ParkedCount: math.MaxInt, ParkedSince: widestInstant, HomesCount: math.MaxInt, RunningCount: math.MaxInt,
		HostLogin: &coord.HostLogin{State: coord.HostLoginState(long), Method: long, Account: long, Detail: long,
			CheckedAt: widestInstant}}
	for range 4 * coord.MaxModelParkedSeats {
		model.Parked = append(model.Parked, coord.ParkedSeat{Seat: seat, Since: widestInstant})
	}
	name := func(i int) string { return fmt.Sprintf("%0*d", coord.MaxFleetHomeNameBytes, i) }
	for i := range widestFleetWork {
		model.Homes = append(model.Homes, coord.FleetHome{Fleet: name(i), Subject: name(i), Job: name(i),
			At: widestInstant})
	}
	for i := range widestFleetWork {
		model.Running = append(model.Running, coord.FleetRunning{Fleet: name(i), Subject: name(i), Job: name(i),
			Since: widestInstant})
	}
	for range 4 * coord.MaxModelLimits {
		model.Limits = append(model.Limits, coord.ModelLimit{Window: long, Hours: math.MaxInt, Model: long,
			State: long, Used: &used, ResetsAt: widestInstant, Seen: widestInstant})
	}
	growth := &coord.GrowthSignals{}
	for range 4 * coord.MaxGrowthSignals {
		growth.ClockSkews = append(growth.ClockSkews, coord.GrowthClockSkew{Product: long, Vendor: long,
			OffsetSeconds: math.MinInt, Since: widestInstant})
		growth.Deprecations = append(growth.Deprecations, coord.GrowthDeprecation{Product: long, Version: long,
			Retires: widestInstant, Since: widestInstant})
	}
	return coord.NodeStatusRecord{Node: "node-9", Epoch: math.MaxInt64, At: widestInstant,
		MCP: []coord.MCPServerStatus{mcp}, Models: []coord.ModelStatus{model},
		Runners: &coord.RunnerStatus{Direct: cell, Container: cell},
		Infra: coord.NodeInfra{Address: long, Cluster: long, BrokerServers: servers, ObjectsBackend: long,
			Bucket: long, Region: long, Endpoint: long},
		ResumesBlocked: &coord.ResumesBlocked{Runs: math.MaxInt, Seat: long, Kind: long, Since: widestInstant},
		Growth:         growth,
	}
}

// EVERY ROW CLASS'S WIDEST RECORD FITS ITS BOUND, as the store encodes it:
// a node's own row with every runner cell, infra text and server at what its
// writer keeps, an MCP row with every count, text and note at its most, a
// model row likewise, and a growth row of two full lists, every text stored
// at exactly its bound. Each row's texts are held to their bounds AS STORED,
// so the whole row is held to the sum, under coord.MaxNodeStatusRowBytes; an
// MCP row past it only by its unlisted keys, which are cut by count.
//
// Mutation: raise MaxGrowthSignals to 35, and the growth row is refused
// (16,652 bytes).
func TestEveryNodeStatusRowsWidestRecordFitsItsBound(t *testing.T) {
	t.Parallel()
	rows, err := widestRecord(0).Rows()
	if err != nil {
		t.Fatalf("the widest record's rows: %v", err)
	}
	seen := map[coord.NodeStatusClass]int{}
	for _, row := range rows {
		seen[row.Key.Class] = len(row.Value)
		if len(row.Value) > coord.MaxNodeStatusRowBytes {
			t.Errorf("the widest %s row is stored as %d bytes, past its class's %d", row.Key.Class,
				len(row.Value), coord.MaxNodeStatusRowBytes)
		}
	}
	if len(seen) != len(coord.NodeStatusClasses) {
		t.Fatalf("premise: the widest record wrote rows of %v, want every class", seen)
	}
	// THE PREMISE: each text is at its bound as stored, never cut shorter.
	read := coord.AssembleNodeStatusRecords(rows, nil)[0]
	for what, got := range map[string][2]int{
		"an MCP error":        {len(read.MCP[0].Error), coord.MaxMCPErrorBytes},
		"a sit-out's words":   {len(read.Models[0].SittingOut.Said), coord.MaxModelSaidBytes},
		"a growth product":    {len(read.Growth.ClockSkews[0].Product), coord.MaxGrowthSignalBytes},
		"an infra address":    {len(read.Infra.Address), coord.MaxInfraText},
		"a runner cell's fix": {len(read.Runners.Container.Fix), coord.MaxModelSaidBytes},
	} {
		if got[0] != got[1] {
			t.Errorf("premise: %s is stored as %d bytes, want its bound %d", what, got[0], got[1])
		}
	}
	t.Logf("the widest rows are stored as %v bytes", seen)
}

// AN MCP ROW'S UNLISTED KEYS ARE KEPT WHOLE, the first by sort, as many as its
// row holds beside its widest other parts, and counted where not all are: a
// key cut short would name a tool nobody wrote. A list that fits travels whole
// and uncounted, and the reader reads the count back.
//
// Mutation: drop the count, and the cut list reads as whole.
func TestAnMCPRowsUnlistedKeysAreKeptWholeAndCountedPastTheRow(t *testing.T) {
	t.Parallel()
	r := widestRecord(1000)
	rows, err := r.Rows()
	if err != nil {
		t.Fatalf("a record of 1,000 unlisted keys is refused: %v", err)
	}
	got := coord.AssembleNodeStatusRecords(rows, nil)[0].MCP[0]
	want := r.MCP[0].Unlisted
	if len(got.Unlisted) == 0 || len(got.Unlisted) == len(want) || got.UnlistedCount != len(want) ||
		!slices.Equal(got.Unlisted, want[:len(got.Unlisted)]) {
		t.Errorf("1,000 unlisted keys read back as %d of them, counted %d, want the first whole and 1,000",
			len(got.Unlisted), got.UnlistedCount)
	}
	if got.Error == "" || got.Wait == "" || len(got.Refused) != coord.MaxMCPToolNotes ||
		len(got.Shadowed) != coord.MaxMCPToolNotes {
		t.Errorf("the keys crowded out the row's other parts: %+v", got)
	}
	for _, row := range rows {
		if row.Key.Class == coord.NodeStatusMCP && len(row.Value) > coord.MaxNodeStatusRowBytes {
			t.Errorf("the MCP row is stored as %d bytes, past %d", len(row.Value), coord.MaxNodeStatusRowBytes)
		}
	}

	few := coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{{Server: "jira",
		Unlisted: []string{"jira_add_coment", "jira_close"}}}}
	if got := recorded(t, few).MCP[0]; !slices.Equal(got.Unlisted, few.MCP[0].Unlisted) || got.UnlistedCount != 0 {
		t.Errorf("two unlisted keys read back as %v counted %d, want both, uncounted", got.Unlisted,
			got.UnlistedCount)
	}
}

// widestFleetWork is how many homes and calls in flight the widest record's
// model row carries: past what any row holds at their widest names.
const widestFleetWork = 256

// A MODEL ROW'S FLEET WORK IS KEPT AS FAR AS ITS ROW HOLDS, AND COUNTED WHOLE:
// an entry with its widest work in flight and homes beside the row's widest
// other parts names its work in flight first, in order, then its homes, as
// many of each as the row holds, and each list's whole count where it names
// fewer, the writer's own where it is the larger, so a reader knows by how
// much it under-reports. Lists that fit travel whole and uncounted.
//
// Mutation: keep every entry in fitFleetWork whatever the row holds, and the
// widest record's model row is refused as past its class; count what the
// lists name rather than what the writer had, and the counts are wrong.
func TestAModelRowsFleetWorkIsKeptAsFarAsItsRowHoldsAndCounted(t *testing.T) {
	t.Parallel()
	r := widestRecord(0)
	rows, err := r.Rows()
	if err != nil {
		t.Fatalf("the widest record's rows: %v", err)
	}
	got, want := coord.AssembleNodeStatusRecords(rows, nil)[0].Models[0], r.Models[0]
	if len(got.Running) == 0 || got.RunningCount != math.MaxInt || got.Running[0].Subject != want.Running[0].Subject ||
		got.HomesCount != math.MaxInt || len(got.Homes) >= widestFleetWork {
		t.Errorf("the widest model row names %d running of %d counted, %d homes of %d counted, want the "+
			"opening of each that fits and every list's whole count", len(got.Running), got.RunningCount,
			len(got.Homes), got.HomesCount)
	}
	if len(got.Limits) != coord.MaxModelLimits || got.SittingOut == nil || got.HostLogin == nil {
		t.Errorf("the fleet work crowded out the row's other parts: %+v", got)
	}

	// SHORT NAMES PAST WHAT THE ROW HOLDS: cut by bytes, counted by what the
	// writer had.
	many := coord.ModelStatus{Key: "default"}
	for i := range 1000 {
		many.Running = append(many.Running, coord.FleetRunning{Fleet: "opus", Subject: fmt.Sprint(i),
			Job: "execute", Since: widestInstant})
		many.Homes = append(many.Homes, coord.FleetHome{Fleet: "opus", Subject: fmt.Sprint(i), Job: "execute",
			At: widestInstant})
	}
	read := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1, Models: []coord.ModelStatus{many}}).Models[0]
	if len(read.Running) == 0 || len(read.Running) >= 1000 || read.RunningCount != 1000 || read.HomesCount != 1000 {
		t.Errorf("a thousand short calls and homes read back as %d running counted %d and %d homes counted %d, "+
			"want the opening of each counted a thousand", len(read.Running), read.RunningCount, len(read.Homes),
			read.HomesCount)
	}

	few := coord.NodeStatusRecord{Node: "n1", Epoch: 1, Models: []coord.ModelStatus{{Key: "default",
		Running: []coord.FleetRunning{{Fleet: "opus", Subject: "sre", Job: "execute", Since: widestInstant}},
		Homes:   []coord.FleetHome{{Fleet: "opus", Subject: "sre", Job: "execute", At: widestInstant}}}}}
	if got := recorded(t, few).Models[0]; len(got.Running) != 1 || len(got.Homes) != 1 ||
		got.RunningCount != 0 || got.HomesCount != 0 {
		t.Errorf("one running and one home read back as %d and %d, counted %d and %d, want both whole, "+
			"uncounted", len(got.Running), len(got.Homes), got.RunningCount, got.HomesCount)
	}
	few.Models[0].RunningCount, few.Models[0].HomesCount = 300, 40
	if got := recorded(t, few).Models[0]; len(got.Running) != 1 || got.RunningCount != 300 || got.HomesCount != 40 {
		t.Errorf("a writer counting 300 running and 40 homes read back as %d running counted %d, homes "+
			"counted %d, want its own counts", len(got.Running), got.RunningCount, got.HomesCount)
	}
}

// A TEXT IS HELD TO ITS BOUND AS STORED, and keeps the longest cut that fits
// it: a text in bytes JSON writes as six (a control character, `<`), or a mix
// of them and plain and multibyte text, is cut to the longest marked prefix,
// whole runes, whose stored form is within the bound, which a search of every
// budget confirms; one within it travels as it was, whatever its own bytes.
//
// Mutation: measure the cut in its own bytes, and a six-fold sentence is
// stored at six times its bound; scale the budget down once instead of
// bisecting, and the cut is two escapes short.
func TestARowsTextIsHeldToItsBoundAsStored(t *testing.T) {
	t.Parallel()
	longest := func(s string, bound int) string {
		best := ""
		for budget := range bound + 1 {
			if c := textcut.Within(s, budget); len(c) > len(best) && jsonBytes(c) <= bound {
				best = c
			}
		}
		return best
	}
	for name, text := range map[string]string{
		"six-fold": sixFold(4 * coord.MaxMCPErrorBytes),
		"html":     strings.Repeat("<a&b>", coord.MaxMCPErrorBytes),
		"mixed":    strings.Repeat("é\x01\"\u2028", coord.MaxMCPErrorBytes),
		"plain":    strings.Repeat("a", 4*coord.MaxMCPErrorBytes),
	} {
		r := coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{{Server: "jira", Error: text}}}
		got := recorded(t, r).MCP[0].Error
		if want := longest(text, coord.MaxMCPErrorBytes); got != want || !strings.HasSuffix(got, "…") {
			t.Errorf("a %s error is stored as %d bytes (%d of its own), want the longest cut that fits %d: %d",
				name, jsonBytes(got), len(got), coord.MaxMCPErrorBytes, jsonBytes(want))
		}
	}
	within := strings.Repeat("é", coord.MaxMCPErrorBytes/2)
	r := coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{{Server: "wiki", Error: within}}}
	if got := recorded(t, r).MCP[0].Error; got != within {
		t.Errorf("an error within its bound as stored was cut to %q", got)
	}
}

// jsonBytes is how many bytes JSON writes s in, its quotes aside.
func jsonBytes(s string) int {
	raw, _ := json.Marshal(s)
	return len(raw) - 2
}

// A ROW'S KEY NAMES ITS CLASS, ITS NODE AND ITS NAME, escaped, and reads back
// as written; a key of another shape, a class nobody writes, a named class
// without its name or an own row with one is no row. A node id carrying a dot
// is one segment, so one node's filter never matches another's rows.
//
// Mutation: join the name to the node in one segment of a named class's
// key, and the keys do not read back.
func TestANodeStatusRowsKeyRoundTripsAndRefusesEveryOtherShape(t *testing.T) {
	t.Parallel()
	for _, k := range []coord.NodeStatusRowKey{
		{Class: coord.NodeStatusOwn, Node: "n1"},
		{Class: coord.NodeStatusGrowth, Node: "pod.example.com"},
		{Class: coord.NodeStatusMCP, Node: "n1", Name: "jira.cloud"},
		{Class: coord.NodeStatusModel, Node: "n1.x", Name: "gpt-5.1"},
	} {
		got, ok := coord.ParseNodeStatusKey(k.Key())
		if !ok || got != k {
			t.Errorf("%+v filed as %q read back as %+v (%t)", k, k.Key(), got, ok)
		}
	}
	for _, key := range []string{"n1", "node_status.n1.x", "mcp_status.n1", "seat_status.n1",
		"model_status.n1.a.b", coord.DocumentKey("node_status", "")} {
		if got, ok := coord.ParseNodeStatusKey(key); ok {
			t.Errorf("the key %q read as a row %+v", key, got)
		}
	}
	if (coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "n1.x", Name: "a"}).Key() ==
		(coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "n1", Name: "x"}).Key() {
		t.Error("two nodes' rows share a key")
	}
}

// A NODE IS COUNTED WHOLE OR NOT AT ALL from a listing's passes, which are not
// one instant: a node whose own row is gone from the second read of the own
// rows (it stopped between them), or moved to another epoch (it was claimed
// anew), or any of whose rows is at a later epoch than its own (its writer
// began its next epoch), is not counted, never counted with part of its rows.
// A node whose own row was only rewritten at its epoch between the two reads
// is counted whole.
//
// Mutation: drop the reread check, or join a later-epoch row's node at its
// own epoch, and a case counts a partial node.
func TestANodeReadAcrossPassesIsCountedWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	rowsOf := func(r coord.NodeStatusRecord) []coord.NodeStatusRow {
		t.Helper()
		rows, err := r.Rows()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	two := func(epoch int64) coord.NodeStatusRecord {
		return coord.NodeStatusRecord{Node: "n1", Epoch: epoch, MCP: []coord.MCPServerStatus{
			{Server: "a", Started: 1}, {Server: "b", Failed: 1}}}
	}
	own := func(rows []coord.NodeStatusRow) []coord.NodeStatusRow {
		return slices.DeleteFunc(slices.Clone(rows), func(r coord.NodeStatusRow) bool {
			return r.Key.Class != coord.NodeStatusOwn
		})
	}
	at5, at6 := rowsOf(two(5)), rowsOf(two(6))
	// a is rewritten at 6 between the own pass and the MCP pass; b is not.
	moving := append([]coord.NodeStatusRow{at6[0]}, at5[1:]...)
	for name, c := range map[string]struct {
		rows, reread []coord.NodeStatusRow
		counted      bool
	}{
		"a row at a later epoch than its own":   {moving, own(at5), false},
		"its own row gone from the reread":      {at5, []coord.NodeStatusRow{}, false},
		"its own row moved epoch in the reread": {at5, own(at6), false},
		"its own row rewritten at its epoch": {at5, own(rowsOf(coord.NodeStatusRecord{Node: "n1", Epoch: 5,
			At: widestInstant})), true},
	} {
		got := coord.AssembleNodeStatusRecords(c.rows, c.reread)
		if counted := len(got) == 1; counted != c.counted || (counted && len(got[0].MCP) != 2) {
			t.Errorf("%s: assembled %+v, want counted %t and whole", name, got, c.counted)
		}
	}
}
