package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
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
	got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
		writerTurn(t), builtin.CodeRebaseChangeTool, args)
	writes, _ := writer.asked()
	if got.Failed || len(writes) != 1 {
		t.Fatalf("the rebase answered %q, sending %d writes", got.Output, len(writes))
	}
	w := writes[0]
	if w.Kind != code.WriteRebaseChange || w.Ref != "refs/heads/feature" || w.ExpectedHead != revHead(3) ||
		w.Change != 7 || w.Principal.Kind != code.PrincipalAgent || w.Principal.Run.TurnID == "" ||
		w.Validate() != nil {
		t.Errorf("the rebase is %+v, want site!7's source rebased at revision 3's head", w)
	}
	if out := decoded(t, got); out["commit"] != writtenOID || out["previous"] != revHead(3) {
		t.Errorf("answered %q, want the commit and the head it moved from", got.Output)
	}

	for name, c := range map[string]struct {
		res   code.EngineWriteResult
		class tools.Refusal
		says  string
	}{
		"a conflict": {code.EngineWriteResult{Refusal: code.WriteConflict, Detail: "site!7's commit 1234567 (2 of " +
			"2) does not apply cleanly", Conflicts: []code.Conflict{{Path: "conf.yaml", Kind: "CONFLICT (contents)",
			Marked: "<<<<<<< ours"}}}, tools.RefusalInvalid, "conf.yaml"},
		"a merge among its commits": {code.EngineWriteResult{Refusal: code.WriteMergeCommit,
			Detail: "site!7's commit 89abcde merges others"}, tools.RefusalInvalid, "89abcde"},
	} {
		res := c.res
		writer := &codeWrites{write: func(code.EngineWrite) (code.EngineWriteResult, error) { return res, nil }}
		got := callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newChangeHost(siteChange()), writer)),
			writerTurn(t), builtin.CodeRebaseChangeTool, args)
		if !got.Failed || tools.RefusalOf(got) != c.class || !strings.Contains(got.Output, c.says) {
			t.Errorf("%s answered %q as %s, want %s naming %q", name, got.Output, tools.RefusalOf(got), c.class,
				c.says)
		}
	}

	none := &codeWrites{}
	got = callWrite(t, everyGrant(), codeRegistry(t, writeDeps(newChangeHost(siteChange()), none)), writerTurn(t),
		builtin.CodeRebaseChangeTool, map[string]any{"change": "site!7", "expected_head": "abc"})
	if writes, _ := none.asked(); !got.Failed || len(writes) != 0 {
		t.Errorf("a rebase at no whole head answered %q, sending %d writes", got.Output, len(writes))
	}
}
