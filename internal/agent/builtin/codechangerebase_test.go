package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/tools"
)

// A REBASE IS ONE ENGINE WRITE OF THE CHANGE'S SOURCE: code_rebase_change of
// site!7 at the head its caller read is one rebase_change of the change's
// source branch, named by its number, as the calling seat in its turn, a
// write every party takes, answered with the commit its branch moved to
// and the head it moved from. A conflict is answered with its paths, fenced,
// nothing changed; a rebase of a merge, invalid; and a head that is no whole
// id is refused before anything is asked.
//
// Mutation: rebase the change's target rather than its source → red.
func TestARebaseIsOneEngineWriteOfTheSource(t *testing.T) {
	t.Parallel()
	args := map[string]any{"change": "site!7", "expected_head": revHead(3)}
	writer := &codeWrites{}
	got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
		writerTurn(t), builtin.CodeRebaseChangeTool, args)
	writes, _ := writer.asked()
	if got.Failed || len(writes) != 1 {
		t.Fatalf("the rebase answered %q, sending %d writes", got.Output, len(writes))
	}
	w := writes[0]
	if w.Kind != gitlet.WriteRebaseChange || w.Ref != "refs/heads/feature" || w.ExpectedHead != revHead(3) ||
		w.Change != 7 || w.Principal.Kind != gitlet.PrincipalAgent || w.Principal.Run.TurnID == "" ||
		w.Validate() != nil {
		t.Errorf("the rebase is %+v, want site!7's source rebased at revision 3's head", w)
	}
	if out := decoded(t, got); out["commit"] != writtenOID || out["previous"] != revHead(3) {
		t.Errorf("answered %q, want the commit and the head it moved from", got.Output)
	}

	for name, c := range map[string]struct {
		res   gitlet.EngineWriteResult
		class tools.Refusal
		says  string
	}{
		"a conflict": {gitlet.EngineWriteResult{Refusal: gitlet.WriteConflict, Detail: "site!7's commit 1234567 (2 of " +
			"2) does not apply cleanly", Conflicts: []gitlet.Conflict{{Path: "conf.yaml", Kind: "CONFLICT (contents)",
			Marked: "<<<<<<< ours"}}}, tools.RefusalInvalid, "conf.yaml"},
		"a merge among its commits": {gitlet.EngineWriteResult{Refusal: gitlet.WriteMergeCommit,
			Detail: "site!7's commit 89abcde merges others"}, tools.RefusalInvalid, "89abcde"},
	} {
		res := c.res
		writer := &codeWrites{write: func(gitlet.EngineWrite) (gitlet.EngineWriteResult, error) { return res, nil }}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
			writerTurn(t), builtin.CodeRebaseChangeTool, args)
		if !got.Failed || tools.RefusalOf(got) != c.class || !strings.Contains(got.Output, c.says) {
			t.Errorf("%s answered %q as %s, want %s naming %q", name, got.Output, tools.RefusalOf(got), c.class,
				c.says)
		}
	}

	none := &codeWrites{}
	got = callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(siteChange()), none)), writerTurn(t),
		builtin.CodeRebaseChangeTool, map[string]any{"change": "site!7", "expected_head": "abc"})
	if writes, _ := none.asked(); !got.Failed || len(writes) != 0 {
		t.Errorf("a rebase at no whole head answered %q, sending %d writes", got.Output, len(writes))
	}
}

// A REBASE OF AN ENDED CHANGE IS A CONFLICT, as a merge of one is and every
// code tool's write of one: the change is in a state that refuses the rebase
// until somebody reopens it, or for good once merged, which internal/mcp's one
// rule classes conflict, never invalid; nothing is written.
//
// Mutation: refuse it as invalid again → red.
func TestARebaseOfAnEndedChangeIsAConflict(t *testing.T) {
	t.Parallel()
	args := map[string]any{"change": "site!7", "expected_head": revHead(3)}
	for _, state := range []gitlet.ChangeState{gitlet.ChangeStateClosed, gitlet.ChangeStateMerged} {
		ended := siteChange()
		ended.detail.State = state
		none := &codeWrites{}
		got := callWrite(everyGrant(), t, codeRegistry(t, writeDeps(newChangeHost(ended), none)), writerTurn(t),
			builtin.CodeRebaseChangeTool, args)
		if writes, _ := none.asked(); !got.Failed || tools.RefusalOf(got) != tools.RefusalConflict ||
			len(writes) != 0 {
			t.Errorf("a rebase of a %s change answered %q as %s, sending %d writes; want conflict and none",
				state, got.Output, tools.RefusalOf(got), len(writes))
		}
	}
}
