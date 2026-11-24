package sandbox

import (
	"fmt"
	"slices"
	"testing"
)

// A CAPTURE KEPT TRIMMED SAYS SO AND IS COUNTED ONCE: the take's word on its
// archive's trim is stamped on the capture the log records and the row keeps,
// and the meter is told of it once, by its harness, as it lands; a capture
// taken whole is stamped and counted neither.
//
// Mutation: leave the trim off the stamp, count a whole capture, or count
// where a capture is taken rather than where it lands, and a case goes red.
func TestACaptureKeptTrimmedSaysSoAndIsCountedOnce(t *testing.T) {
	t.Parallel()
	for _, trimmed := range []bool{true, false} {
		t.Run(fmt.Sprint(trimmed), func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				spy := &carrySpy{}
				rig, writes, _ := keepingRigWith(t, func(o *CoordinatorOptions) { o.Carries = spy })
				if trimmed {
					rig.runner.Session.Archive.Trim = ArchiveTrim{RecordBytes: 300 << 20, KeptBytes: 40 << 20}
				}
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
					HarnessVersion: "2.1.310", Captured: &session.Archive})
				collectHeld(t, rig, "t1")
				captures := writes.captured()
				if len(captures) != 1 || captures[0].CaptureRecord.Stamp.Trimmed != trimmed {
					t.Fatalf("recorded %+v, want one capture trimmed %t", captures, trimmed)
				}
				if run := rig.get("t1"); run.Kept == nil || run.Kept.Trimmed != trimmed {
					t.Errorf("the row keeps %+v, want the capture trimmed %t", run.Kept, trimmed)
				}
				var want []string
				if trimmed {
					want = []string{"claude-code"}
				}
				if got := spy.trims(); !slices.Equal(got, want) {
					t.Errorf("the meter was told of trims %q, want %q", got, want)
				}
			})
		})
	}
}
