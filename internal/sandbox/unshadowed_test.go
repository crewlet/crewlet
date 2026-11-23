package sandbox

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"
)

// A CAPTURE KEEPS WHETHER ITS RUN WAS UNSHADOWED: the take puts the box's word
// on the stamp and the row's kept capture carries it; a relaunch's reader is
// handed the kept capture's word on the archive it reads, never true where
// the capture says nothing; and an older build's write of the row keeps it.
//
// Mutation: leave the word off the stamp, off the archive HoldingsOf hands,
// or off the wire, and a check goes red.
func TestACaptureKeepsWhetherItsRunWasUnshadowed(t *testing.T) {
	t.Parallel()
	for _, unshadowed := range []bool{true, false} {
		t.Run("stamped", func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, writes, _ := keepingRig(t)
				rig.runner.Session.Archive.RunStart = RunStart{Began: true}
				rig.runner.Session.Archive.Unshadowed = unshadowed
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
					HarnessVersion: "2.1.310", Captured: &session.Archive})
				collectHeld(t, rig, "t1")
				captures := writes.captured()
				if len(captures) != 1 || captures[0].CaptureRecord.Stamp.RunUnshadowed != unshadowed {
					t.Fatalf("recorded %+v, want one capture unshadowed %t", captures, unshadowed)
				}
				if run := rig.get("t1"); run.Kept == nil || run.Kept.RunUnshadowed != unshadowed {
					t.Errorf("the row keeps %+v, want the capture unshadowed %t", run.Kept, unshadowed)
				}
			})
		})
		t.Run("read", func(t *testing.T) {
			t.Parallel()
			w := newHoldingsWorldWith(t, false, func(c *SessionCapture) { c.RunUnshadowed = unshadowed })
			got, err := w.rig.coordinator.HoldingsOf(t.Context(), "t1", w.kept.LaunchSeq, w.kept.CaptureSeq,
				askingAbout("toolu_01a"), w.reader)
			if err != nil || !got.Read {
				t.Fatalf("HoldingsOf = %+v, %v; want a reading", got, err)
			}
			if asked, _ := w.reader.reads(); len(asked) != 1 || asked[0].Unshadowed != unshadowed {
				t.Errorf("the reader was handed %+v, want once the kept capture's word, unshadowed %t", asked,
					unshadowed)
			}
		})
	}
	raw, err := json.Marshal(SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, RunUnshadowed: true})
	if err != nil {
		t.Fatal(err)
	}
	declared := wireKeys(reflect.TypeFor[SessionCapture]())
	if !declared["run_unshadowed"] {
		t.Fatalf("the capture declares no key run_unshadowed: %v", declared)
	}
	older := maps.Clone(declared)
	delete(older, "run_unshadowed")
	type plain SessionCapture
	var read plain
	extra, err := unmarshalCarrying(raw, &read, older)
	if err != nil {
		t.Fatal(err)
	}
	read.RunUnshadowed = false
	written, err := marshalCarrying(read, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back SessionCapture
	if err := json.Unmarshal(written, &back); err != nil || !back.RunUnshadowed {
		t.Errorf("after an older build's write the capture is %+v (%v), want it unshadowed: %s", back, err, written)
	}
}
