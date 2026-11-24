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
)

// sixFold is n bytes JSON writes as six each, the widest any text stores as.
func sixFold(n int) string { return strings.Repeat("\x01", n) }

// widestRecord is a node's status with every field at the most its writer
// could be handed: every text far past its bound in bytes JSON writes as six,
// every list past its bound, every count and epoch at its largest and every
// instant to the nanosecond.
func widestRecord(unlisted int) coord.NodeStatusRecord {
	long := sixFold(4096)
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
	used := -math.SmallestNonzeroFloat64
	model := coord.ModelStatus{Key: "default", Plan: long, SignIn: long, Resolves: true,
		SittingOut: &coord.ModelSitOut{Until: widestInstant, ResetsAt: widestInstant, Since: widestInstant,
			Window: long, Said: long},
		LastAnswer:  widestInstant,
		LastFailure: &coord.ModelFailure{Kind: long, At: widestInstant, Said: long},
		ParkedCount: math.MaxInt, ParkedSince: widestInstant,
		HostLogin: &coord.HostLogin{State: coord.HostLoginState(long), Method: long, Account: long, Detail: long,
			CheckedAt: widestInstant}}
	for range 4 * coord.MaxModelParkedSeats {
		model.Parked = append(model.Parked, coord.ParkedSeat{Seat: seat, Since: widestInstant})
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
		Runners: &coord.RunnerStatus{Direct: cell, Container: cell, E2B: cell},
		Infra: coord.NodeInfra{Address: long, Cluster: long, BrokerServers: servers, ObjectsBackend: long,
			Bucket: long, Region: long, Endpoint: long},
		ResumesBlocked: &coord.ResumesBlocked{Runs: math.MaxInt, Seat: long, Kind: long, Since: widestInstant},
		Growth:         growth,
	}
}

// EVERY ROW CLASS'S WIDEST RECORD FITS ITS BOUND, as the store encodes it:
// a node's own row with every runner cell, infra text and server at what its
// writer keeps, an MCP row with every count, text and note at its most, a
// model row likewise, and a growth row of two full lists, each text in bytes
// JSON writes as six. Each row's texts are held to their bounds AS STORED, so
// the whole row is held to the sum, under coord.MaxNodeStatusRowBytes; an MCP
// row past it only by its unlisted keys, which are cut by count.
//
// Mutation: cut a text to its bound in its own bytes (fitWithin as
// textcut.Within), and the widest rows are refused; raise MaxGrowthSignals to
// 40, and the growth row is (16,942 bytes).
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
	got := coord.AssembleNodeStatusRecords(rows)[0].MCP[0]
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

// A TEXT IS HELD TO ITS BOUND AS STORED: a text in bytes JSON writes as six is
// cut to what fits the bound once written, marked, whole runes; one within it
// travels as it was, whatever its own bytes.
//
// Mutation: measure the cut in its own bytes, and the six-fold sentence is
// stored at six times its bound.
func TestARowsTextIsHeldToItsBoundAsStored(t *testing.T) {
	t.Parallel()
	r := coord.NodeStatusRecord{Node: "n1", Epoch: 1, MCP: []coord.MCPServerStatus{
		{Server: "jira", Error: sixFold(coord.MaxMCPErrorBytes)},
		{Server: "wiki", Error: strings.Repeat("é", coord.MaxMCPErrorBytes/2)},
	}}
	got := recorded(t, r).MCP
	stored, _ := json.Marshal(got[0].Error)
	if len(stored)-2 > coord.MaxMCPErrorBytes || !strings.HasSuffix(got[0].Error, "…") {
		t.Errorf("a six-fold error of %d bytes is stored as %d, want within %d and marked",
			coord.MaxMCPErrorBytes, len(stored)-2, coord.MaxMCPErrorBytes)
	}
	if got[1].Error != r.MCP[1].Error {
		t.Errorf("an error within its bound as stored was cut to %q", got[1].Error)
	}
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
