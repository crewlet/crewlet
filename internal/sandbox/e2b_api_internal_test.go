package sandbox

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A BOX'S TRAFFIC TOKEN IS READ AS PRESENT AND NEVER KEPT: the engine sends it
// nowhere, so it needs only to know E2B minted one, and a field that holds a
// bool leaves no handle, log line or row able to carry the token. A value of
// any other JSON kind is no answer, never a decode error, which would lose the
// id of the box the create just made and leave it running. The field is read
// by reflection, so an edit that keeps the string builds, and fails here.
//
// Mutation: keep the string in [answered], and the kind check fails; read the
// key's presence alone, and the empty and null rows fail.
func TestATrafficTokenIsReadAsPresentAndNeverKept(t *testing.T) {
	t.Parallel()
	for answer, want := range map[string]bool{
		`{"sandboxID": "sbx1", "trafficAccessToken": "x"}`:  true,
		`{"sandboxID": "sbx1", "trafficAccessToken": ""}`:   false,
		`{"sandboxID": "sbx1", "trafficAccessToken": null}`: false,
		`{"sandboxID": "sbx1"}`:                             false,
		`{"sandboxID": "sbx1", "trafficAccessToken": 7}`:    false,
	} {
		var box e2bBox
		if err := json.Unmarshal([]byte(answer), &box); err != nil {
			t.Errorf("%s: %v", answer, err)
			continue
		}
		field := reflect.ValueOf(box).FieldByName("TrafficToken")
		if field.Kind() != reflect.Bool {
			t.Fatalf("e2bBox.TrafficToken is a %s, which can hold what E2B answered; want a bool", field.Kind())
		}
		if field.Bool() != want || box.SandboxID != "sbx1" {
			t.Errorf("%s read as a token answered %v of box %q; want %v of sbx1", answer,
				field.Bool(), box.SandboxID, want)
		}
	}
}
