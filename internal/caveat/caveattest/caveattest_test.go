package caveattest_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A MODEL'S ANSWER FOR THE PERSON IS EXEMPT BY TOOL AND PATH, NEVER BY KEY.
// answer_knowledge's `answer_md` is an answer written for the person who
// asked, markdown included, and is not walked; the same key on another tool,
// or nested deeper in answer_knowledge's own answer, carries whatever that
// tool put there, and is.
func TestOnlyAnswerKnowledgesAnswerIsExempt(t *testing.T) {
	t.Parallel()
	model := "Call `get_work_item` on ENG-7, then comment_on_work_item."
	for name, c := range map[string]struct {
		tool   string
		answer map[string]any
		walked bool
	}{
		"answer_knowledge's own answer": {builtin.AnswerKnowledgeTool,
			map[string]any{"answer_md": model}, false},
		"answer_knowledge's answer, nested by the act route": {builtin.AnswerKnowledgeTool,
			map[string]any{"receipt": map[string]any{"answer_md": model}}, false},
		"another tool's answer_md": {worklet.CreateWorkItemTool,
			map[string]any{"answer_md": model}, true},
		"another tool's answer_md, nested": {worklet.CreateWorkItemTool,
			map[string]any{"receipt": map[string]any{"answer_md": model}}, true},
		"answer_md deeper in answer_knowledge's answer": {builtin.AnswerKnowledgeTool,
			map[string]any{"sources": []any{map[string]any{"answer_md": model}}}, true},
	} {
		if got := len(caveattest.AnswerProblems(c.tool, c.answer)) > 0; got != c.walked {
			t.Errorf("%s: walked %v, want %v", name, got, c.walked)
		}
	}
}
