package estate

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A GATE RECORD GOES TO ITS OWN LOG'S PARTITION, AND NAMES A LOG OF THE LAYOUT.
//
// `statelog.gate` is published by a node serving the partition of the one log
// the record is for, so its partition function answers that partition and no
// other — and refuses a record naming a log the running layout does not carry:
// another layout's, a partition the layout lacks, a domain with no log there.
func TestAGateRecordGoesToItsOwnLogsPartition(t *testing.T) {
	t.Parallel()
	layout := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 4, Domains: []string{"tracker", "vectors"}},
		{Space: statelog.SpacePages, Partitions: 1, Domains: []string{"pages"}},
	}}
	args := GateArgs{Layout: 1, Domain: "tracker", Partition: "tracker.003", Node: "node-b",
		By: "ops", OpID: "op"}
	got, err := GatePartitions(layout, args)
	if err != nil || len(got) != 1 || got[0] != (statelog.PartitionID{Space: statelog.SpaceTracker, Index: 3}) {
		t.Fatalf("the gate record of tracker@tracker.003 goes to %v (%v), want tracker.003 alone", got, err)
	}
	for name, change := range map[string]func(*GateArgs){
		"another layout's log":          func(a *GateArgs) { a.Layout = 2 },
		"a partition the layout lacks":  func(a *GateArgs) { a.Partition = "tracker.004" },
		"a domain with no log there":    func(a *GateArgs) { a.Domain = "pages" },
		"a name that is no partition's": func(a *GateArgs) { a.Partition = "tracker.3" },
		"no partition":                  func(a *GateArgs) { a.Partition = "" },
	} {
		bad := args
		change(&bad)
		if _, err := GatePartitions(layout, bad); !errors.Is(err, ErrGateArgs) {
			t.Errorf("a gate record naming %s answered %v, want ErrGateArgs", name, err)
		}
	}
}
