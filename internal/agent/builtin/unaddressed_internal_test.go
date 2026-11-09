package builtin

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A COMMENT THAT LEAVES THE ASSIGNEE UNASKED IS WARNED ABOUT, IN THE ROUTING'S
// OWN TERMS, AND ONE THAT ASKS THEM NEVER IS. Another agent's remark naming
// nobody is warned that it does not wake the assignee at all, and so is a
// reply in a thread the assignee wrote in (ADR-0113); a reply in a thread
// where the assignee still owes an answer, that it wakes them without asking
// them anything; a person's remark is the ask, so it carries no warning. An answer
// closes a question and expects no reply, so warning its author that nobody is
// asked to reply is wrong.
//
// Mutation: decide the warning from the comment alone rather than from the
// record it publishes, and the person's remark or the thread reply is red.
func TestOnlyACommentThatLeavesTheAssigneeUnaskedIsWarned(t *testing.T) {
	t.Parallel()
	question := "c-question"
	task := worklet.Task{Assignee: "backend-engineer"}
	agent := Actor{Handle: "qa", Kind: worklet.AuthorAgent, WrittenFor: types.ForTurn(nil)}
	person := Actor{Handle: "miles", Kind: worklet.AuthorHuman}
	const (
		quiet = iota
		notWoken
		wokenUnasked
	)
	for name, c := range map[string]struct {
		task    worklet.Task
		actor   Actor
		comment worklet.Comment
		thread  []string
		want    int
		owing   []string
	}{
		"another agent's remark naming nobody":       {task, agent, worklet.Comment{}, nil, notWoken, nil},
		"an agent's reply in the assignee's thread":  {task, agent, worklet.Comment{}, []string{"backend-engineer"}, notWoken, nil},
		"an agent's reply in somebody else's thread": {task, agent, worklet.Comment{}, []string{"designer"}, notWoken, nil},
		"an agent's reply where the assignee owes an answer": {task, agent, worklet.Comment{},
			[]string{"backend-engineer"}, wokenUnasked, []string{"backend-engineer"}},
		"a person's remark naming nobody": {task, person, worklet.Comment{}, nil, quiet, nil},
		"an answer to a question":         {task, agent, worklet.Comment{Answers: &question}, nil, quiet, nil},
		"an answer naming its choice":     {task, person, worklet.Comment{Answers: &question, Choice: "yes"}, nil, quiet, nil},
		"a question":                      {task, agent, worklet.Comment{Ask: "backend-engineer"}, nil, quiet, nil},
		"a remark that mentions":          {task, agent, worklet.Comment{Mentions: []string{"backend-engineer"}}, nil, quiet, nil},
		"the assignee's own remark":       {task, Actor{Handle: "backend-engineer", Kind: worklet.AuthorAgent, WrittenFor: types.ForTurn(nil)}, worklet.Comment{}, nil, quiet, nil},
		"a task nobody holds":             {worklet.Task{}, agent, worklet.Comment{}, nil, quiet, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			comment := c.comment
			comment.AuthorKind = c.actor.Kind
			notify := &worklet.Notify{Kind: worklet.ChangeComment, Mentions: comment.Mentions,
				Snapshot: worklet.Snapshot{
					Assignee: c.task.Assignee, CommentAuthorKind: c.actor.Kind,
					CommentAsk: comment.Ask, ThreadParticipants: c.thread,
					ThreadOwing: c.owing,
				}}
			got := unaddressedCaveat(c.task, c.actor, &comment, notify)
			switch {
			case c.want == quiet:
				if !got.IsZero() {
					t.Fatalf("caveat = %+v, want none", got)
				}
				return
			case got.IsZero():
				t.Fatal("no caveat, want the assignee warned about")
			case !strings.Contains(got.Person(), "backend-engineer"):
				t.Errorf("the caveat does not name the assignee: %q", got.Person())
			}
			woken := !strings.Contains(got.Agent(), "is not woken")
			if woken != (c.want == wokenUnasked) {
				t.Errorf("the caveat says %q, want woken = %v", got.Agent(), c.want == wokenUnasked)
			}
		})
	}
}
