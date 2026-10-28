package builtin

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/worklet"
)

// A COMMENT THAT WAKES THE ASSIGNEE WITHOUT ASKING THEM ANYTHING IS WARNED
// ABOUT, AND AN ANSWER NEVER IS: an answer closes a question and expects no
// reply, so warning its author that nobody is asked to reply is wrong.
func TestOnlyAnUnaddressedRemarkIsWarnedThatNobodyIsAskedToReply(t *testing.T) {
	t.Parallel()
	question := "c-question"
	task := worklet.Task{Assignee: "backend-engineer"}
	person := Actor{Handle: "miles"}
	for name, c := range map[string]struct {
		task    worklet.Task
		actor   Actor
		comment worklet.Comment
		warned  bool
	}{
		"a remark naming nobody":      {task, person, worklet.Comment{}, true},
		"an answer to a question":     {task, person, worklet.Comment{Answers: &question}, false},
		"an answer naming its choice": {task, person, worklet.Comment{Answers: &question, Choice: "yes"}, false},
		"a question":                  {task, person, worklet.Comment{Ask: "backend-engineer"}, false},
		"a remark that mentions":      {task, person, worklet.Comment{Mentions: []string{"backend-engineer"}}, false},
		"the assignee's own remark":   {task, Actor{Handle: "backend-engineer"}, worklet.Comment{}, false},
		"a task nobody holds":         {worklet.Task{}, person, worklet.Comment{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			comment := c.comment
			got := unaddressedCaveat(c.task, c.actor, &comment)
			if got.IsZero() == c.warned {
				t.Errorf("caveat = %+v, want warned=%v", got, c.warned)
			}
			if c.warned && !strings.Contains(got.Person(), "backend-engineer") {
				t.Errorf("the caveat does not name the assignee: %q", got.Person())
			}
		})
	}
}
