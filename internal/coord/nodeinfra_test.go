package coord_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A SUMMARY READS BACK AS IT WAS WRITTEN, through the bytes a node's status
// record is stored as, and a record that carries none reads back as a node
// saying nothing: a zero summary, never one with an address of "".
func TestANodesInfraSummaryRoundTripsThroughItsStatusRecord(t *testing.T) {
	t.Parallel()
	want := coord.NodeInfra{Address: "10.0.0.4", Cluster: "acme", BrokerServers: []string{"nats-1.internal", "nats-2.internal"},
		ObjectsBackend: "s3", Bucket: "acme-files", Region: "eu-west-1", Endpoint: "s3.eu-west-1.amazonaws.com"}
	got := recorded(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1, Infra: want})
	if !reflect.DeepEqual(got.Infra, want) {
		t.Errorf("summary read back as %+v, want %+v", got.Infra, want)
	}
	if _, sent := wireOf(t, coord.NodeStatusRecord{Node: "n1", Epoch: 1})["infra"]; sent {
		t.Error("a node that says nothing of what it runs on writes a summary")
	}
	if (coord.NodeInfra{}).Meta() != nil {
		t.Error("a node that says nothing writes a summary")
	}
}

// A SUMMARY IS BOUNDED, so one node's row cannot grow with a pasted list.
func TestANodesInfraSummaryIsBounded(t *testing.T) {
	t.Parallel()
	servers := make([]string, 40)
	for i := range servers {
		servers[i] = strings.Repeat("h", 400)
	}
	meta := coord.NodeInfra{Address: strings.Repeat("a", 4000), BrokerServers: servers}.Meta()
	if got := len(meta["address"].(string)); got != coord.MaxInfraText {
		t.Errorf("an address of 4000 bytes is written as %d, want %d", got, coord.MaxInfraText)
	}
	list := meta["broker_servers"].([]any)
	if len(list) != coord.MaxInfraServers || len(list[0].(string)) != coord.MaxInfraText {
		t.Errorf("40 servers are written as %d of %d bytes", len(list), len(list[0].(string)))
	}
}
