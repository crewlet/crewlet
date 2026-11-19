package builtin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REVERT'S MERGE IS STEP TWO (M5-D25): code_revert_change sends one revert
// of the named change on the seat's revert branch and, asked to merge, a
// merge of the change its push opened, at the revert's commit, into the
// reverted change's target, as step 2 of the revert's operation. A refused
// merge is a residue beside the opened change, never a refusal of the revert.
// The branch's deletion is the Gitlet node's to decide: the merge sends no
// delete_source and reads no gates.
//
// Mutations: merge under the revert's own operation, answer a refused merge
// as the call's refusal, or decide the branch's deletion in the tool → red.
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

// A SECOND REVERT NAMES THE ONE UNDER WAY: its branch exists, so the write is refused stale,
// naming the change the reverted one's read says reverts it.
// Mutation: drop the read of what reverts it → the refusal names no change, red.
func TestASecondRevertNamesTheOneUnderWay(t *testing.T) {
	t.Parallel()
	writer := &codeWrites{}
	writer.write = func(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
		return gitlet.EngineWriteResult{Command: gitlet.Command{Ref: w.Ref, Old: writtenOID, New: mainOID,
			Result: gitlet.ResultNG, Reason: gitlet.RefusalStale, Detail: "the branch exists"}}, nil
	}
	f := siteChange()
	f.detail.State = gitlet.ChangeStateMerged
	f.detail.RevertedBy = &gitlet.ChangeName{Repo: "site", Ref: "site!9", Title: "Revert site!7",
		State: gitlet.ChangeStateOpen}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(f), writer)), writerTurn(t),
		builtin.CodeRevertChangeTool, map[string]any{"change": "site!7"})
	if !got.Failed || !strings.Contains(got.Output, "a revert of site!7 is under way: site!9, open") {
		t.Fatalf("a second revert answered %q, want it refused naming site!9", got.Output)
	}
}
