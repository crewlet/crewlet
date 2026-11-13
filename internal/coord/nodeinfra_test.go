package coord_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// A SUMMARY READS BACK AS IT WAS WRITTEN, through the JSON a presence row is
// stored as, and a row that carries none says so rather than reading as a
// node with nothing to say.
func TestANodesInfraSummaryRoundTripsThroughItsPresenceRow(t *testing.T) {
	t.Parallel()
	want := coord.NodeInfra{Address: "10.0.0.4", Cluster: "acme", BrokerServers: []string{"nats-1.internal", "nats-2.internal"},
		ObjectsBackend: "s3", Bucket: "acme-files", Region: "eu-west-1", Endpoint: "s3.eu-west-1.amazonaws.com"}
	raw, err := json.Marshal(map[string]any{coord.InfraKey: want.Meta()})
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	got, ok := coord.InfraFromMeta(meta)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("summary read back as %+v (%v), want %+v", got, ok, want)
	}
	if _, ok = coord.InfraFromMeta(map[string]any{"roles": []any{"data"}}); ok {
		t.Error("a row with no summary read as one")
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
