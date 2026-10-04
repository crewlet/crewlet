package coord_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A LAYOUT-0 ROW AND A LAYOUT-0 FLOOR ARE THE RECORDS EVERY EARLIER BUILD WROTE.
//
// Both are keyed by a log's key, which does not carry the layout, so each
// carries the layout beside it — and at layout 0 it is omitted, because a
// running fleet's register already holds these records and a build that added
// a field to them would rewrite every row and every floor it touched with bytes
// no earlier build wrote. Pinned as literal bytes, since "the same fields" is a
// claim a reordering or a renamed tag would pass.
func TestALayoutZeroRowAndFloorAreByteForByteTheEarlierRecords(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	row := coord.NodePositions{
		NodeID: "node-a", At: at, EngineVersion: "v0.0.0-test",
		Domains: map[string]coord.DomainPosition{
			"tracker": {Seq: 7, Generation: 1, AppliedThrough: 7, RecordVersion: 4},
		},
	}
	floor := coord.TrimFloor{Domain: "tracker", Generation: 1, TrimTo: 5, Floor: 5, At: at, By: "node-a"}
	// AND ITS SNAPSHOT ON THE ROW, where every earlier build reported layout
	// 0's one artefact: a partition's report, a map epoch and a log's state
	// are all absent from a layout-0 row.
	snapshotted := row
	snapshotted.SnapshotBytes, snapshotted.SnapshotSkip = 4096, "lagging"

	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"a positions row": {row, `{"node_id":"node-a","at":"2026-09-28T12:00:00Z",` +
			`"engine_version":"v0.0.0-test","domains":{"tracker":{"seq":7,"generation":1,` +
			`"applied_through":7,"record_version":4}}}`},
		"a positions row with its snapshot": {snapshotted, `{"node_id":"node-a",` +
			`"at":"2026-09-28T12:00:00Z","engine_version":"v0.0.0-test","domains":{"tracker":` +
			`{"seq":7,"generation":1,"applied_through":7,"record_version":4}},` +
			`"snapshot_bytes":4096,"snapshot_skip":"lagging"}`},
		"a trim floor": {floor, `{"domain":"tracker","generation":1,"trim_to":5,"floor":5,` +
			`"at":"2026-09-28T12:00:00Z","by":"node-a"}`},
	} {
		got, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s at layout 0 is written as\n\t%s\nand every earlier build wrote\n\t%s",
				name, got, tc.want)
		}
	}

	// AND ANY OTHER LAYOUT SAYS SO, or the key beside it names a log of
	// every layout at once.
	row.Layout, floor.Layout = 1, 1
	for name, value := range map[string]any{"a positions row": row, "a trim floor": floor} {
		got, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if decoded["layout"] != float64(1) {
			t.Errorf("%s at layout 1 carries layout %v on the wire: %s", name, decoded["layout"], got)
		}
	}
}

// A READER NEVER TAKES ANOTHER LAYOUT'S POSITION OR FLOOR FOR ITS OWN LOG'S.
//
// `tracker@tracker.007` is the same key in layout 1 and in a repartitioned
// layout 2, so a reader keyed by it would read the other layout's position as
// this log's — a trim counting it, a fence clearing a write against it. The
// filter keeps every row, because its node is still a node the trim and the
// seal count, and drops what the row says about logs of another layout.
func TestAReaderSeesOnlyItsOwnLayoutsPositionsAndFloors(t *testing.T) {
	t.Parallel()
	const key = "tracker@tracker.007"
	pos := map[string]coord.DomainPosition{key: {Seq: 9, AppliedThrough: 9}}
	rows := []coord.NodePositions{
		{NodeID: "on-one", Layout: 1, Domains: pos},
		{NodeID: "on-two", Layout: 2, Domains: pos},
	}
	read := coord.PositionsIn(rows, 1)
	if len(read) != 2 {
		t.Fatalf("the filter kept %d rows of 2 — a node on another layout is still "+
			"a node the fleet counts", len(read))
	}
	if _, held := read[0].Domains[key]; !held {
		t.Error("layout 1's own row lost its position")
	}
	if _, held := read[1].Domains[key]; held {
		t.Error("layout 2's position was read as layout 1's for the same key")
	}
	if _, held := rows[1].Domains[key]; !held {
		t.Error("the filter emptied the caller's own row rather than its copy")
	}

	floors := coord.FloorsIn([]coord.TrimFloor{
		{Domain: key, Layout: 1, Floor: 10},
		{Domain: key, Layout: 2, Floor: 500},
		{Domain: "tracker", Floor: 700},
	}, 1)
	if len(floors) != 1 || floors[0].Floor != 10 {
		t.Errorf("layout 1 reads the floors %+v, want only its own", floors)
	}
}

// NO RECORD NAMES A LAYOUT BEFORE THE FIRST.
//
// A layout number counts repartitions from 0, so a negative one names no
// layout at all — and read through the filters it would match no reader,
// leaving a row whose positions nobody counts and a floor nobody honours,
// written without a word. Both are refused at the write instead.
func TestARecordNamingANegativeLayoutIsRefused(t *testing.T) {
	t.Parallel()
	row := coord.NodePositions{NodeID: "node-a", Layout: -1,
		Domains: map[string]coord.DomainPosition{"tracker": {Seq: 1, AppliedThrough: 1}}}
	if err := row.Validate(); err == nil {
		t.Error("a positions row at layout -1 was accepted")
	}
	floor := coord.TrimFloor{Domain: "tracker", Layout: -1, TrimTo: 1, Floor: 1}
	if err := floor.Validate(); err == nil {
		t.Error("a trim floor at layout -1 was accepted")
	}
	row.Layout, floor.Layout = 0, 0
	if err := row.Validate(); err != nil {
		t.Errorf("the control row was refused: %v", err)
	}
	if err := floor.Validate(); err != nil {
		t.Errorf("the control floor was refused: %v", err)
	}
}

// A PARTITION'S REPORT HAS ONE PLACE PER LAYOUT.
//
// Layout 0's one partition reports its snapshot on the row itself — where every
// earlier build wrote it, which is what keeps a layout-0 row the bytes they
// wrote — and every partitioned layout reports each partition in its own entry.
// A row carrying both would give a reader two answers for one partition, so the
// writer is refused either mixture, and [coord.NodePositions.Report] reads each
// layout's from its one place.
func TestAPartitionsReportHasOnePlacePerLayout(t *testing.T) {
	t.Parallel()
	domains := map[string]coord.DomainPosition{"tracker": {Seq: 3, AppliedThrough: 3}}
	for name, tc := range map[string]struct {
		row     coord.NodePositions
		refused bool
	}{
		"layout 0 on the row": {row: coord.NodePositions{NodeID: "n", Domains: domains,
			SnapshotBytes: 10, SnapshotSkip: "lagging"}},
		"layout 0 per partition": {refused: true, row: coord.NodePositions{NodeID: "n",
			Domains:    domains,
			Partitions: map[string]coord.PartitionReport{"estate.000": {State: "serving"}}}},
		"layout 0 at a map epoch": {refused: true, row: coord.NodePositions{NodeID: "n",
			Domains: domains, MapEpoch: 3}},
		"layout 1 per partition": {row: coord.NodePositions{NodeID: "n", Layout: 1, MapEpoch: 3,
			Domains: map[string]coord.DomainPosition{"tracker@tracker.001": {Seq: 3, AppliedThrough: 3}},
			Partitions: map[string]coord.PartitionReport{"tracker.001": {State: "serving",
				SnapshotBytes: 10, SnapshotSkip: "recent"}}}},
		"layout 1 on the row": {refused: true, row: coord.NodePositions{NodeID: "n", Layout: 1,
			Domains: domains, SnapshotBytes: 10}},
		"layout 1 skipping on the row": {refused: true, row: coord.NodePositions{NodeID: "n",
			Layout: 1, Domains: domains, SnapshotSkip: "lagging"}},
		"an unnamed partition": {refused: true, row: coord.NodePositions{NodeID: "n", Layout: 1,
			Domains: domains, Partitions: map[string]coord.PartitionReport{"": {State: "serving"}}}},
	} {
		err := tc.row.Validate()
		if refused := err != nil; refused != tc.refused {
			t.Errorf("%s: refused %v (%v), want %v", name, refused, err, tc.refused)
		}
	}

	zero := coord.NodePositions{NodeID: "n", Domains: domains, SnapshotBytes: 10, SnapshotSkip: "lagging"}
	if r, ok := zero.Report("estate.000"); !ok || r.SnapshotBytes != 10 || r.SnapshotSkip != "lagging" {
		t.Errorf("layout 0's one partition reads %+v (%v) off a row whose snapshot is 10 "+
			"bytes and skipping `lagging`", r, ok)
	}
	one := coord.NodePositions{NodeID: "n", Layout: 1, SnapshotBytes: 99,
		Partitions: map[string]coord.PartitionReport{"tracker.001": {State: "serving", SnapshotBytes: 10}}}
	if r, ok := one.Report("tracker.001"); !ok || r.SnapshotBytes != 10 || r.State != "serving" {
		t.Errorf("layout 1's tracker.001 reads %+v (%v), want its own entry", r, ok)
	}
	if r, ok := one.Report("tracker.002"); ok {
		t.Errorf("a partition the row does not report reads %+v", r)
	}
}
