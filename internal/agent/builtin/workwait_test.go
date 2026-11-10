package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// AN UPDATE SETS AN ITEM ASIDE UNTIL A DATE ONLY WITH WHY: `wait_until` is the
// item's start and needs a one-line `wait_reason`; a reason alone, a wait and
// a start together, or a date that has passed are refused with nothing
// written, and null ends the wait.
func TestAWaitUntilADateSaysWhy(t *testing.T) {
	t.Parallel()
	const reason = "the vendor's window opens then"
	for _, c := range []struct {
		name    string
		args    map[string]any
		refusal string
	}{
		{"a wait with its reason", map[string]any{"wait_until": "2031-04-20", "wait_reason": reason}, ""},
		{"a wait without a reason", map[string]any{"wait_until": "2031-04-20"}, "without `wait_reason`"},
		{"a blank reason", map[string]any{"wait_until": "2031-04-20", "wait_reason": "  "}, "without `wait_reason`"},
		{"a reason without a wait", map[string]any{"wait_reason": reason}, "without `wait_until`"},
		{"a wait and a start", map[string]any{"wait_until": "2031-04-20", "start": "2031-04-21",
			"wait_reason": reason}, "both `wait_until` and `start`"},
		{"a date that has passed", map[string]any{"wait_until": "2020-01-01", "wait_reason": reason}, "has passed"},
		{"a reason of two lines", map[string]any{"wait_until": "2031-04-20", "wait_reason": "the window\nopens"},
			"not one line"},
		{"the wait ended", map[string]any{"wait_until": nil}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			c.args["item"] = "ENG-1"
			got := callWork(t, reg, builtin.UpdateWorkItemTool, c.args)
			if c.refusal != "" {
				if !got.Failed || !strings.Contains(got.Output, c.refusal) || len(trk.patched) != 0 {
					t.Fatalf("answered %q and wrote %d, want refused saying %q", got.Output,
						len(trk.patched), c.refusal)
				}
				return
			}
			if got.Failed || len(trk.patched) != 1 {
				t.Fatalf("answered %q and wrote %d, want one write", got.Output, len(trk.patched))
			}
			patch := trk.patched[0]
			if c.args["wait_until"] == nil {
				if !patch.ClearStart || patch.StartAt != nil || patch.WaitReason != nil {
					t.Errorf("an ended wait wrote clear_start %v, start %v and reason %v, want the start "+
						"cleared by its flag", patch.ClearStart, patch.StartAt, patch.WaitReason)
				}
				return
			}
			if patch.StartAt == nil || patch.StartAt.Format("2006-01-02") != "2031-04-20" ||
				patch.WaitReason == nil || *patch.WaitReason != reason {
				t.Errorf("the wait wrote start %v and reason %v, want 2031-04-20 and %q", patch.StartAt,
					patch.WaitReason, reason)
			}
		})
	}
}
