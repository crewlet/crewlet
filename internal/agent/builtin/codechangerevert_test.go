package builtin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REVERT'S MERGE IS STEP TWO (M5-D25): code_revert_change sends ONE revert
// of the change it names on the seat's own revert branch, and — asked to
// merge — a merge of the change its push opened, at the commit the revert
// made, into the reverted change's target, under the revert's operation's
// step 2; a merge refused is said beside the opened change as a residue,
// never as a refusal of the revert that landed.
//
// Its branch's deletion is left to the Gitlet node that judges the merge, as an
// unsaid delete_branch is: the merge says no word of it and nothing reads
// the target's gates for it.
//
// Mutations: merge under the revert's own operation → red; answer a refused
// merge as the call's refusal → red; decide the branch's deletion in the tool
// → red.
func TestARevertsMergeIsStepTwo(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]bool{"merged": false, "refused": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{}
			writer.write = func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
				if w.Kind == gitlet.WriteRevertChange {
					return gitlet.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: writtenOID,
						Change: &gitlet.ChangeAnswer{ChangeID: siteChangeID, Number: 8, Opened: true}}, nil
				}
				if refuse {
					return gitlet.EngineWriteResult{Merge: &gitlet.MergeAnswer{Refusal: gitlet.MergeRefusalBusy,
						Detail: "the committer is busy"}}, nil
				}
				return gitlet.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: mainOID,
					Merge: &gitlet.MergeAnswer{}}, nil
			}
			host := newChangeHost(siteChange())
			got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(host, writer)), writerTurn(t),
				builtin.CodeRevertChangeTool, map[string]any{"change": "site!7", "merge": true})
			if got.Failed {
				t.Fatalf("the revert answered %q", got.Output)
			}
			writes, _ := writer.asked()
			if len(writes) != 2 {
				t.Fatalf("the revert sent %+v, want the revert and its merge", writes)
			}
			revert, merge := writes[0], writes[1]
			if revert.Kind != gitlet.WriteRevertChange || revert.Change != 7 ||
				!strings.HasPrefix(revert.Ref, "refs/heads/seats/") || !strings.HasSuffix(revert.Ref, "/revert-7") {
				t.Errorf("the revert sent %+v, want site!7 reverted on the seat's revert-7", revert)
			}
			if merge.Kind != gitlet.WriteMergeChange || merge.OpID != statelog.StepOpID(revert.OpID, "2") ||
				merge.Change != 8 || merge.ExpectedHead != writtenOID || merge.Ref != "refs/heads/main" {
				t.Errorf("the revert's merge is %+v, want site!8 at %s into main as step 2 of %s", merge,
					writtenOID, revert.OpID)
			}
			reads, _, _ := host.asked()
			gated := slices.ContainsFunc(reads, func(q gitlet.ReadQuery) bool { return q.Gates })
			if merge.DeleteSource != nil || gated {
				t.Errorf("the revert's merge sent delete_source %v, the gates read %v: its branch's deletion is "+
					"the Gitlet node's to decide", merge.DeleteSource, gated)
			}
			out := decoded(t, got)
			residue, _ := out["residue"].(string)
			switch {
			case out["opened"] != "site!8":
				t.Errorf("answered %q, want site!8 opened", got.Output)
			case !refuse && (out["merge"] != "merged" || residue != ""):
				t.Errorf("answered %q, want site!8 merged", got.Output)
			case refuse && !strings.HasPrefix(residue, "reverted; site!8 not merged"):
				t.Errorf("answered %q, want the merge's refusal as a residue", got.Output)
			}
		})
	}
}
