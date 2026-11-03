package backuptest

import (
	"encoding/json"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
)

// THE RESTORE REQUEST IS THE SERVER'S, with its two fields kept raw.
//
// restoreRequest spells server.JSApiStreamRestoreRequest's tags itself so the
// manifest's configuration and state reach the server byte for byte; a tag
// that drifted from the server's would send a request the server reads as
// naming no configuration, and it restores a stream with none — this decodes
// what restoreRequest encodes with the server's own type.
func TestTheRestoreRequestIsTheServersOwnShape(t *testing.T) {
	t.Parallel()
	config, err := json.Marshal(server.StreamConfig{
		Name: "CREWLET_PROBE_LOG", Subjects: []string{"crewlet.probe.log.>"}, Storage: server.FileStorage,
		Metadata: map[string]string{"crewlet.created_at": "2031-04-02T03:00:00.418226517Z"},
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := json.Marshal(server.StreamState{FirstSeq: 7, LastSeq: 1000, Msgs: 994})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(restoreRequest{Config: config, State: state})
	if err != nil {
		t.Fatal(err)
	}
	var decoded server.JSApiStreamRestoreRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("the server cannot read the request: %v", err)
	}
	if decoded.Config.Name != "CREWLET_PROBE_LOG" ||
		decoded.Config.Metadata["crewlet.created_at"] != "2031-04-02T03:00:00.418226517Z" ||
		decoded.State.FirstSeq != 7 || decoded.State.LastSeq != 1000 || decoded.State.Msgs != 994 {
		t.Fatalf("the server reads the request as %+v / %+v", decoded.Config, decoded.State)
	}
}
