package executor

import (
	"fmt"
	"testing"
)

// grokHookHeads are the heads of [grokHookAnswers]' reminders for a hook at
// place 0, the run's where the box has no hook of its own for the event.
var grokHookHeads = []string{fmt.Sprintf(grokHookAnswers[0].head, 0), fmt.Sprintf(grokHookAnswers[1].head, 0)}

// THE RUN'S HOOK IS READ AT ANY PLACE ITS BOX PUTS IT, and nothing else is: a
// reminder of the home's settings' hook at place 0, 2 or 9999 opens the
// run's words, and one at no place, one past the most digits read, another
// scope or another event's name does not. Mutations, each red: read place 0
// alone; read no digits as a place; read past grokPlaceDigits.
func TestAGrokHooksAnswerIsReadAtAnyPlace(t *testing.T) {
	t.Parallel()
	const words = "## A note\n\nUse main."
	for name, tc := range map[string]struct {
		text string
		want bool
	}{
		"place 0":         {fmt.Sprintf(grokHookAnswers[0].head, 0) + words + grokReminderTail, true},
		"place 2":         {fmt.Sprintf(grokHookAnswers[1].head, 2) + words + grokReminderTail, true},
		"place 9999":      {fmt.Sprintf(grokHookAnswers[0].head, 9999) + words + grokReminderTail, true},
		"no place":        {"<system-reminder>\nContext from PostToolUse hook 'user:post_tool_use[].hooks[0]':\n" + words, false},
		"too many digits": {fmt.Sprintf(grokHookAnswers[0].head, 10000) + words + grokReminderTail, false},
		"a repository's":  {"<system-reminder>\nContext from PostToolUse hook 'project:post_tool_use[0].hooks[0]':\n" + words, false},
		"another event":   {"<system-reminder>\nContext from Stop hook 'user:stop[1].hooks[0]':\n" + words, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, ok := grokHookText("system_reminder", tc.text)
			if ok != tc.want || ok && got != words {
				t.Errorf("read %q, %t; want the words %t", got, ok, tc.want)
			}
		})
	}
}
