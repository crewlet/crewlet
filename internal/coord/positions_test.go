package coord_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// A POSITIONS ROW AND A TRIM FLOOR ARE THE PINNED BYTES.
//
// Both are records the whole fleet shares in the positions bucket, written by
// one build and read by another through every rolling upgrade, and neither has
// an age: a row or a floor written today is read for the life of the
// deployment. So what a current node writes is pinned as literal bytes, field
// for field — every field of a row with its snapshot and of a floor with a
// blocked term — because "the same fields" is a claim a reordering, a renamed
// tag or a field nobody meant to put on the wire would all pass. A key here is
// a log's domain name and nothing else.
func TestAPositionsRowAndAFloorAreThePinnedBytes(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	stored := time.Date(2026, 9, 28, 11, 59, 0, 0, time.UTC)
	snapped := time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)

	row := coord.NodePositions{
		NodeID: "node-a", At: at, EngineVersion: "v0.0.0-test",
		Domains: map[string]coord.DomainPosition{
			"tracker": {
				Seq: 7, Generation: 1, AppliedThrough: 6,
				StreamCreatedAt: created, CheckpointStoredAt: stored,
				SnapshotSeq: 5, SnapshotGeneration: 1, SnapshotAt: snapped,
				Deferred: 1, RecordVersion: 13, LogDiverged: true,
			},
			"pages": {Seq: 3, Generation: 1, AppliedThrough: 3, RecordVersion: 2},
		},
		SnapshotBytes: 4096, SnapshotSkip: "lagging",
	}
	floor := coord.TrimFloor{
		Domain: "tracker", Generation: 1, TrimTo: 0, Floor: 5,
		BlockedBy: "applied", BlockedSince: snapped,
		Terms: []coord.TrimTerm{
			{Name: "applied", Detail: "the live data nodes could not be listed"},
			{Name: "max_age", Seq: 9, Known: true},
			{Name: "wake_feed", Known: true, Absent: true},
		},
		At: at, By: "node-a",
	}

	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"a positions row": {row, `{"node_id":"node-a","at":"2026-09-28T12:00:00Z",` +
			`"engine_version":"v0.0.0-test","domains":{` +
			`"pages":{"seq":3,"generation":1,"applied_through":3,"record_version":2},` +
			`"tracker":{"seq":7,"generation":1,"applied_through":6,` +
			`"stream_created_at":"2026-09-01T08:00:00Z",` +
			`"checkpoint_stored_at":"2026-09-28T11:59:00Z",` +
			`"snapshot_seq":5,"snapshot_generation":1,"snapshot_at":"2026-09-28T03:00:00Z",` +
			`"deferred":1,"record_version":13,"log_diverged":true}},` +
			`"snapshot_bytes":4096,"snapshot_skip":"lagging"}`},
		"a row with nothing but positions": {coord.NodePositions{NodeID: "node-b", At: at,
			Domains: map[string]coord.DomainPosition{"tracker": {Seq: 1, Generation: 1, AppliedThrough: 1}}},
			`{"node_id":"node-b","at":"2026-09-28T12:00:00Z","domains":{"tracker":` +
				`{"seq":1,"generation":1,"applied_through":1}}}`},
		"a blocked trim floor": {floor, `{"domain":"tracker","generation":1,"trim_to":0,"floor":5,` +
			`"blocked_by":"applied","blocked_since":"2026-09-28T03:00:00Z","terms":[` +
			`{"name":"applied","seq":0,"known":false,"detail":"the live data nodes could not be listed"},` +
			`{"name":"max_age","seq":9,"known":true},` +
			`{"name":"wake_feed","seq":0,"known":true,"absent":true}],` +
			`"at":"2026-09-28T12:00:00Z","by":"node-a"}`},
	} {
		got, err := json.Marshal(tc.value)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s is written as\n\t%s\nand the pinned record is\n\t%s", name, got, tc.want)
		}
	}
}
