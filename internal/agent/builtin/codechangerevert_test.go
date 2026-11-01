package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A REVERT'S MERGE IS STEP TWO (M5-D25): code_revert_change sends ONE revert
// of the change it names on the seat's own revert branch, and — asked to
// merge — a merge of the change its push opened, at the commit the revert
// made, into the reverted change's target, under the revert's operation's
// step 2; a merge refused is said beside the opened change as a residue,
// never as a refusal of the revert that landed.
//
// Mutations: merge under the revert's own operation → red; answer a refused
// merge as the call's refusal → red.
func TestARevertsMergeIsStepTwo(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]bool{"merged": false, "refused": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &codeWrites{}
			writer.write = func(w code.EngineWrite) (code.EngineWriteResult, error) {
				if w.Kind == code.WriteRevertChange {
					return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: writtenOID,
						Change: &code.ChangeAnswer{ChangeID: siteChangeID, Number: 8, Opened: true}}, nil
				}
				if refuse {
					return code.EngineWriteResult{Merge: &code.MergeAnswer{Refusal: code.MergeRefusalBusy,
						Detail: "the committer is busy"}}, nil
				}
				return code.EngineWriteResult{Outcome: statelog.OutcomeApplied, Commit: mainOID,
					Merge: &code.MergeAnswer{}}, nil
			}
			got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
				writerTurn(t), builtin.CodeRevertChangeTool, map[string]any{"change": "site!7", "merge": true})
			if got.Failed {
				t.Fatalf("the revert answered %q", got.Output)
			}
			writes, _ := writer.asked()
			if len(writes) != 2 {
				t.Fatalf("the revert sent %+v, want the revert and its merge", writes)
			}
			revert, merge := writes[0], writes[1]
			if revert.Kind != code.WriteRevertChange || revert.Change != 7 ||
				!strings.HasPrefix(revert.Ref, "refs/heads/seats/") || !strings.HasSuffix(revert.Ref, "/revert-7") {
				t.Errorf("the revert sent %+v, want site!7 reverted on the seat's revert-7", revert)
			}
			if merge.Kind != code.WriteMergeChange || merge.OpID != statelog.StepOpID(revert.OpID, "2") ||
				merge.Change != 8 || merge.ExpectedHead != writtenOID || merge.Ref != "refs/heads/main" {
				t.Errorf("the revert's merge is %+v, want site!8 at %s into main as step 2 of %s", merge,
					writtenOID, revert.OpID)
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
