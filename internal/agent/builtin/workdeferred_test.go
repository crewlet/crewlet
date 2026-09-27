package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DEFERRED READ IS NOT AN INVITATION TO TRY AGAIN.
//
// A node holding a change it cannot apply — one a newer build wrote — refuses
// a read of the item that change covers, and waiting does not clear that: the
// seat's next call asks the same node the same question. So the model is told
// what resolves it and to say it could not check, and never "try again",
// which spends its rounds on a loop that cannot end. A refusal that DOES clear
// by waiting keeps the invitation, and neither is ever an empty result.
//
// Mutation: drop the deferred arm from unservedRead and both tools tell the
// model to try again.
func TestADeferredReadIsNotAnInvitationToTryAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		refusal   *statelog.Refused
		tryAgain  bool
		newerHint bool
	}{
		{"deferred", &statelog.Refused{
			Code: statelog.RefuseDeferred, Level: statelog.ReadSession,
			Detail: "this node retains 1 record(s) covering task i1",
		}, false, true},
		{"behind", &statelog.Refused{
			Code: statelog.RefuseBehind, Level: statelog.ReadSession,
			Detail: "this node has not reached the floor",
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.readErr = tc.refusal
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			for name, args := range map[string]map[string]any{
				builtin.GetWorkItemTool:   {"item": "ENG-1"},
				builtin.CommentOnWorkTool: {"item": "ENG-1", "body": "still right?"},
			} {
				got := callWork(t, reg, name, args)
				if !got.Failed || !strings.Contains(got.Output, "NOT an empty result") {
					t.Errorf("%s on a %s refusal gave %q", name, tc.name, got.Output)
				}
				if said := strings.Contains(got.Output, "Try again"); said != tc.tryAgain {
					t.Errorf("%s on a %s refusal says try again = %v, want %v: %q",
						name, tc.name, said, tc.tryAgain, got.Output)
				}
				if said := strings.Contains(got.Output, "newer build"); said != tc.newerHint {
					t.Errorf("%s on a %s refusal names a newer build = %v, want "+
						"%v: %q", name, tc.name, said, tc.newerHint, got.Output)
				}
			}
		})
	}
}
