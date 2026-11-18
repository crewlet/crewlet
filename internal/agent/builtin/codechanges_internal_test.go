package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY CHANGE TOOL NAMES THE ACCESS IT NEEDS: changeToolNeeds has a row for
// every change read and write and nothing else, so no tool serves at an access
// nobody named. A read's row is read, all its answering node judges
// (gitlet.JudgeRead). A write's row is what its decide holds the colleague to
// (CeilingNeeds), but a branch move's is read: the Gitlet node holds it to the
// push's rule (gitlet.EngineWrite.Ceiling). belowWriteNeeds offers a write at
// no less than its decide's access and below write (CodeWritesWithheld).
//
// Mutations: a row dropped or naming no tool; a read's row or the rebase's
// raised to review; the review's row or its decide's need lowered to read; the
// review offered below write at read → red.
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
		case !need.Valid() || need == gitlet.AccessNone:
			t.Errorf("%s needs %s, which grants nothing a call could be held to", name, need)
		}
		annotations, known := codeAnnotations(name)
		if !known {
			t.Errorf("%s is not classified as a code tool", name)
			continue
		}
		if annotations.ReadOnly == mcp.Yes && need != gitlet.AccessRead {
			t.Errorf("%s is a read and needs %s: a read is judged for whoever it is made for at read, so a "+
				"read's row is read", name, need)
		}
	}
	for name := range changeToolNeeds {
		if !slices.Contains(every, name) {
			t.Errorf("changeToolNeeds names %s, which is no change tool", name)
		}
	}
	// A WRITE'S ROW IS WHAT ITS DECIDE HOLDS A COLLEAGUE TO (gitlet.WriteCeiling):
	// the committer reads it from the write the tool sends, so the two must agree.
	sends := map[string]interface{ CeilingNeeds() gitlet.Access }{
		CodeOpenChangeTool:       gitlet.ChangeProposal{Open: &gitlet.ChangeOpen{}},
		CodeUpdateChangeTool:     gitlet.ChangeProposal{Edit: &gitlet.ChangeEdit{}},
		CodeCloseChangeTool:      gitlet.ChangeProposal{Close: &gitlet.ChangeClose{}},
		CodeRequestReviewTool:    gitlet.ConversationWrite{Reviewers: &gitlet.ReviewerRequest{}},
		CodeReviewChangeTool:     gitlet.ConversationWrite{Review: &gitlet.Review{}},
		CodeCommentOnChangeTool:  gitlet.ConversationWrite{Comment: &gitlet.Comment{}},
		CodeResolveFindingTool:   gitlet.ConversationWrite{FindingResolve: &gitlet.FindingResolve{}},
		CodeApplySuggestionsTool: gitlet.ConversationWrite{FindingResolve: &gitlet.FindingResolve{}},
	}
	// The change writes a Gitlet node decides as a branch move, and nothing else.
	pushes := []string{CodeRebaseChangeTool, CodeRevertChangeTool}
	// The writes whose deciding node judges merge authority from its own chart
	// (M5-D2), never by an access this table names.
	merging := []string{CodeMergeChangeTool, CodeDequeueChangeTool}
	for _, name := range ChangeWrites() {
		write, ok := sends[name]
		switch {
		case slices.Contains(merging, name):
			if ok || changeToolNeeds[name] != gitlet.AccessRead {
				t.Errorf("%s is judged by its deciding node's merge authority, so its row is read and no "+
					"ceiling's access names it: row %s", name, changeToolNeeds[name])
			}
		case slices.Contains(pushes, name):
			if ok || changeToolNeeds[name] != gitlet.AccessRead {
				t.Errorf("%s is a branch move the Gitlet node holds a colleague to by the push's rule, so its row is "+
					"read and no decide's access names it: row %s", name, changeToolNeeds[name])
			}
		case !ok:
			t.Errorf("%s names no write its decide holds a colleague by", name)
		case write.CeilingNeeds() != changeToolNeeds[name]:
			t.Errorf("%s needs %s by changeToolNeeds, and its decide holds a colleague at %s", name,
				changeToolNeeds[name], write.CeilingNeeds())
		}
	}
	// AND WHAT A SEAT BELOW WRITE IS OFFERED (belowWriteNeeds): a change write,
	// at no less than its decide holds a colleague to and less than write, the
	// one tier offered every write whatever this table says.
	for name, need := range belowWriteNeeds {
		switch {
		case !slices.Contains(ChangeWrites(), name):
			t.Errorf("belowWriteNeeds names %s, which is no change write", name)
		case need < changeToolNeeds[name]:
			t.Errorf("a seat below write is offered %s at %s, below the %s changeToolNeeds holds a colleague to",
				name, need, changeToolNeeds[name])
		case need >= gitlet.AccessWrite:
			t.Errorf("belowWriteNeeds names %s at %s, which no tier below write reaches", name, need)
		}
	}
}

// EVERY ID create_work_item MINTS IS ONE A LISTING TAKES AS NAMED: the code
// tools skip the tracker read for a Worklet id only where worklet.IsTaskID
// takes it, so an id minted in another spelling costs every listing a read.
//
// Mutation: createdTaskID spelling its uuid in upper case → red.
func TestACreatedItemsIDIsOneAListingTakesAsNamed(t *testing.T) {
	t.Parallel()
	for project, args := range map[string]map[string]any{
		"ENG": {"title": "the follow-up", "project": "ENG"},
		"OPS": {"title": "another item", "project": "OPS"},
	} {
		if id := createdTaskID(seatInATurn(), CreateWorkItemTool, project, args); !worklet.IsTaskID(id) {
			t.Errorf("create_work_item mints %q, which a listing resolves rather than taking as named", id)
		}
	}
}
