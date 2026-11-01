package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/mcp"
)

// EVERY CHANGE TOOL NAMES THE ACCESS IT NEEDS: the one table of what each
// change tool's call needs on the change's repository, of its caller and of
// the colleague a turn reads for, holds a row for every change tool — the
// reads and the writes — and for nothing else, so a change tool added without
// a row, or a row left behind by one removed, fails here rather than serving
// whoever messaged a seat at an access nobody named. And a READ's row is
// read: the node that answers a read judges the colleague it is made for at
// read and nothing else (code.JudgeRead, through the ceiling every question of
// it carries), so a read naming more would be a need no read holds anybody to.
// A WRITE's row is the access the deciding node holds the colleague its turn
// works for to (code.ChangeProposal.CeilingNeeds,
// code.ConversationWrite.CeilingNeeds), the two one rule — but for a write
// that is a branch move alone, a rebase, which the git node holds the
// colleague to as every branch write (code.EngineWrite.Ceiling, Grant.Under on
// the one ref it moves, the seat's own work spared): a rule of the push and
// no access its decide reads, so its row is read, the least that admits it.
//
// Mutations: a change tool's row dropped — a read's or a write's; a row naming
// a tool that is not one; a read's row raised to review; the review's row, or
// the decide's need for a review, lowered to read; the rebase's row raised to
// review → red.
func TestEveryChangeToolNamesTheAccessItNeeds(t *testing.T) {
	t.Parallel()
	every := slices.Concat(ChangeToolNames(), ChangeWrites())
	if len(ChangeToolNames()) == 0 || len(ChangeWrites()) == 0 {
		t.Fatal("there are no change reads or no change writes, so this walk certifies nothing")
	}
	for _, name := range every {
		need, ok := changeToolNeeds[name]
		switch {
		case !ok:
			t.Errorf("%s names no access in changeToolNeeds", name)
			continue
		case !need.Valid() || need == code.AccessNone:
			t.Errorf("%s needs %s, which grants nothing a call could be held to", name, need)
		}
		annotations, known := codeAnnotations(name)
		if !known {
			t.Errorf("%s is not classified as a code tool", name)
			continue
		}
		if annotations.ReadOnly == mcp.Yes && need != code.AccessRead {
			t.Errorf("%s is a read and needs %s: a read is judged for whoever it is made for at read, so a "+
				"read's row is read", name, need)
		}
	}
	for name := range changeToolNeeds {
		if !slices.Contains(every, name) {
			t.Errorf("changeToolNeeds names %s, which is no change tool", name)
		}
	}
	// A WRITE'S ROW IS THE ACCESS ITS DECIDE HOLDS A COLLEAGUE TO in a turn
	// that colleague woke (code.WriteCeiling): the committer reads it from the
	// write the tool sends, so the two cannot be allowed to disagree.
	sends := map[string]interface{ CeilingNeeds() code.Access }{
		CodeOpenChangeTool:       code.ChangeProposal{Open: &code.ChangeOpen{}},
		CodeUpdateChangeTool:     code.ChangeProposal{Edit: &code.ChangeEdit{}},
		CodeCloseChangeTool:      code.ChangeProposal{Close: &code.ChangeClose{}},
		CodeRequestReviewTool:    code.ConversationWrite{Reviewers: &code.ReviewerRequest{}},
		CodeReviewChangeTool:     code.ConversationWrite{Review: &code.Review{}},
		CodeCommentOnChangeTool:  code.ConversationWrite{Comment: &code.Comment{}},
		CodeResolveFindingTool:   code.ConversationWrite{FindingResolve: &code.FindingResolve{}},
		CodeApplySuggestionsTool: code.ConversationWrite{FindingResolve: &code.FindingResolve{}},
	}
	// The change writes a git node decides as a branch move, and nothing else.
	pushes := []string{CodeRebaseChangeTool}
	// And the writes whose committer judges its asker's merge authority
	// from its own chart (M5-D2), carrying no ceiling its decide reads.
	merging := []string{CodeMergeChangeTool, CodeDequeueChangeTool}
	for _, name := range ChangeWrites() {
		write, ok := sends[name]
		switch {
		case slices.Contains(merging, name):
			if ok || changeToolNeeds[name] != code.AccessRead {
				t.Errorf("%s is judged by its committer's merge authority, so its row is read and no ceiling's "+
					"access names it: row %s", name, changeToolNeeds[name])
			}
		case slices.Contains(pushes, name):
			if ok || changeToolNeeds[name] != code.AccessRead {
				t.Errorf("%s is a branch move the git node holds a colleague to by the push's rule, so its row is "+
					"read and no decide's access names it: row %s", name, changeToolNeeds[name])
			}
		case !ok:
			t.Errorf("%s names no write its decide holds a colleague by", name)
		case write.CeilingNeeds() != changeToolNeeds[name]:
			t.Errorf("%s needs %s by changeToolNeeds, and its decide holds a colleague at %s", name,
				changeToolNeeds[name], write.CeilingNeeds())
		}
	}
}
