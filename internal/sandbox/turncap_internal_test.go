package sandbox

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A JOB'S ROUND CAP SURVIVES A BUILD THAT DOES NOT KNOW IT. Mid-upgrade an
// older build rewrites the row whole on every step (an attach, a mark, a
// park), decoding the job's record into a struct with no such field and
// carrying unknown keys back verbatim in Extra (wirekeys.go). So the cap is a
// wire key of the record's own, `turn_cap`, which that build's rewrite writes
// back and this build's collection still holds the job to.
//
// Mutation: drop a key the reader does not know instead of keeping it in Extra
// (wirekeys.go), or write the cap under no wire key (`json:"-"`), and the
// rewritten row carries no cap.
func TestAJobsRoundCapSurvivesABuildThatDoesNotKnowIt(t *testing.T) {
	t.Parallel()
	run := PendingRun{TurnID: "t1", CommandID: "cmd-1",
		Launch: LaunchRecord{Model: "m", ProviderKey: "head", TurnCap: 7}}
	raw, err := json.Marshal(run.Launch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"turn_cap":7`) {
		t.Fatalf("the job's record is written %s, with no cap under the key an older build carries", raw)
	}

	// THE OLDER BUILD'S DECODE AND ENCODE: this build's codec, declaring every
	// key of the record but the cap's, into a record that holds no cap.
	type plain LaunchRecord
	older := maps.Clone(wireKeys(reflect.TypeFor[LaunchRecord]()))
	delete(older, "turn_cap")
	var held plain
	extra, err := unmarshalCarrying(raw, &held, older)
	if err != nil {
		t.Fatal(err)
	}
	held.TurnCap = 0
	rewritten, err := marshalCarrying(held, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back LaunchRecord
	if err := json.Unmarshal(rewritten, &back); err != nil {
		t.Fatal(err)
	}
	run.Launch = back
	if back.TurnCap != 7 || back.ProviderKey != "head" || run.Handle().TurnCap != 7 {
		t.Errorf("an older build's rewrite reads back as %+v (from %s), handle %+v; want the cap of 7",
			back, rewritten, run.Handle())
	}
}

// AN OLDER BUILD'S OWN LAUNCH HOLDS ITS JOB TO NO CAP OF THIS BUILD'S: its
// relaunch writes the job's record whole, with no `turn_cap`, so the job its
// row names reads 0 and its collection judges it by its harness's cap alone,
// never by a cap it was not handed.
//
// Mutation: have the handle default a record that names no cap to one, and
// the older launch's job is judged by a cap it was never handed.
func TestAnOlderBuildsRelaunchHoldsItsJobToNoCap(t *testing.T) {
	t.Parallel()
	// The row as an older build's relaunch leaves it: the record its launch
	// wrote, which names the model and entry and no cap.
	row := []byte(`{"turn_id":"t1","command_id":"cmd-2","launch_record":{"started_at":"2026-11-24T12:00:00Z",` +
		`"model":"m","provider_key":"head"}}`)
	var run PendingRun
	if err := json.Unmarshal(row, &run); err != nil {
		t.Fatal(err)
	}
	if run.Launch.TurnCap != 0 || run.Handle().TurnCap != 0 || run.Launch.ProviderKey != "head" {
		t.Errorf("an older build's relaunch reads as %+v, handle %+v; want its job's record and no cap",
			run.Launch, run.Handle())
	}
}
