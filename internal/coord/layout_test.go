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

	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"a positions row": {row, `{"node_id":"node-a","at":"2026-09-28T12:00:00Z",` +
			`"engine_version":"v0.0.0-test","domains":{"tracker":{"seq":7,"generation":1,` +
			`"applied_through":7,"record_version":4}}}`},
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
